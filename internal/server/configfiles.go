package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ammonode/daemon/internal/config"
	"github.com/ammonode/daemon/internal/remote"
)

var placeholder = regexp.MustCompile(`{{\s*([\w.\-]+)\s*}}`)

// resolve replaces egg placeholders such as {{server.build.default.port}} or
// {{server.build.env.SERVER_JARFILE}} with real values.
func resolve(value string, c remote.ServerConfiguration) string {
	return placeholder.ReplaceAllStringFunc(value, func(m string) string {
		key := placeholder.FindStringSubmatch(m)[1]
		switch key {
		case "server.build.default.port":
			return strconv.Itoa(c.Allocations.Default.Port)
		case "server.build.default.ip":
			return c.Allocations.Default.IP
		case "server.build.memory":
			return strconv.FormatInt(c.Build.MemoryLimit, 10)
		case "config.docker.interface":
			return config.Get().Docker.Network.Interface
		}
		for _, prefix := range []string{"server.build.env.", "server.environment.", "env."} {
			if strings.HasPrefix(key, prefix) {
				return c.Environment[strings.TrimPrefix(key, prefix)]
			}
		}
		return m
	})
}

func valueString(v interface{}, c remote.ServerConfiguration) string {
	switch t := v.(type) {
	case string:
		return resolve(t, c)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return resolve(string(b), c)
	}
}

// ApplyConfigFiles rewrites each configured file before the server starts.
func ApplyConfigFiles(root string, c remote.ServerConfiguration) error {
	var errs []string
	for _, f := range c.Process.Configs {
		path, err := SafePath(root, f.File)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if err := applyOne(path, f, c); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f.File, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func applyOne(path string, f remote.ConfigFile, c remote.ServerConfiguration) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	switch f.Parser {
	case "properties", "file":
		return replaceLines(path, f, c)
	case "json":
		return replaceStructured(path, f, c, json.Unmarshal, func(v interface{}) ([]byte, error) { return json.MarshalIndent(v, "", "    ") })
	case "yaml", "yml":
		return replaceStructured(path, f, c, yaml.Unmarshal, yaml.Marshal)
	case "ini":
		return replaceIni(path, f, c)
	default:
		return fmt.Errorf("unsupported parser %q", f.Parser)
	}
}

func readLines(path string) ([]string, os.FileMode, error) {
	mode := os.FileMode(0o644)
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, mode, nil
	}
	if err != nil {
		return nil, mode, err
	}
	defer file.Close()
	if st, err := file.Stat(); err == nil {
		mode = st.Mode()
	}
	var lines []string
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, mode, sc.Err()
}

func writeLines(path string, lines []string, mode os.FileMode) error {
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), mode)
}

// replaceLines handles `properties` (key=value) and `file` (line starts with match).
func replaceLines(path string, f remote.ConfigFile, c remote.ServerConfiguration) error {
	lines, mode, err := readLines(path)
	if err != nil {
		return err
	}
	for _, r := range f.Replace {
		val := valueString(r.Value, c)
		found := false
		for i, line := range lines {
			if f.Parser == "properties" {
				k, _, ok := strings.Cut(line, "=")
				if ok && strings.TrimSpace(k) == r.Match {
					lines[i] = r.Match + "=" + val
					found = true
				}
			} else if strings.HasPrefix(line, r.Match) {
				lines[i] = val
				found = true
			}
		}
		if !found && f.Parser == "properties" {
			lines = append(lines, r.Match+"="+val)
		}
	}
	return writeLines(path, lines, mode)
}

func setPath(m map[string]interface{}, path []string, v interface{}) {
	for i, k := range path {
		if i == len(path)-1 {
			m[k] = v
			return
		}
		next, ok := m[k].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			m[k] = next
		}
		m = next
	}
}

func typed(s string) interface{} {
	if s == "true" || s == "false" {
		return s == "true"
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && strings.Contains(s, ".") {
		return f
	}
	return s
}

func replaceStructured(path string, f remote.ConfigFile, c remote.ServerConfiguration, decode func([]byte, interface{}) error, encode func(interface{}) ([]byte, error)) error {
	data := map[string]interface{}{}
	b, err := os.ReadFile(path)
	if err == nil && len(strings.TrimSpace(string(b))) > 0 {
		if err := decode(b, &data); err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, r := range f.Replace {
		var v interface{}
		if s, ok := r.Value.(string); ok {
			v = typed(resolve(s, c))
		} else {
			v = r.Value
		}
		setPath(data, strings.Split(r.Match, "."), v)
	}
	out, err := encode(data)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func replaceIni(path string, f remote.ConfigFile, c remote.ServerConfiguration) error {
	lines, mode, err := readLines(path)
	if err != nil {
		return err
	}
	for _, r := range f.Replace {
		section, key := "", r.Match
		if i := strings.LastIndex(r.Match, "."); i > 0 {
			section, key = r.Match[:i], r.Match[i+1:]
		}
		val := valueString(r.Value, c)
		current := ""
		done := false
		sectionEnd := -1
		for i, line := range lines {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
				if current == section && sectionEnd == -1 {
					sectionEnd = i
				}
				current = strings.Trim(t, "[]")
				continue
			}
			k, _, ok := strings.Cut(t, "=")
			if ok && current == section && strings.TrimSpace(k) == key {
				lines[i] = key + "=" + val
				done = true
			}
		}
		if !done {
			entry := key + "=" + val
			switch {
			case section != "" && current != section && sectionEnd == -1 && !containsSection(lines, section):
				lines = append(lines, "["+section+"]", entry)
			case sectionEnd >= 0:
				lines = append(lines[:sectionEnd], append([]string{entry}, lines[sectionEnd:]...)...)
			default:
				lines = append(lines, entry)
			}
		}
	}
	return writeLines(path, lines, mode)
}

func containsSection(lines []string, section string) bool {
	for _, l := range lines {
		if strings.TrimSpace(l) == "["+section+"]" {
			return true
		}
	}
	return false
}
