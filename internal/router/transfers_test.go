package router

import "testing"

func TestValidUUID(t *testing.T) {
	for id, want := range map[string]bool{
		"3f1c2d4e-5a6b-4c7d-8e9f-0a1b2c3d4e5f": true,
		"3F1C2D4E-5A6B-4C7D-8E9F-0A1B2C3D4E5F": true,
		"../../etc/passwd":                     false,
		"3f1c2d4e-5a6b-4c7d-8e9f-0a1b2c3d4e5":  false,
		"3f1c2d4e_5a6b-4c7d-8e9f-0a1b2c3d4e5f": false,
		"3f1c2d4e-5a6b-4c7d-8e9f-0a1b2c3d4e/f": false,
	} {
		if got := validUUID(id); got != want {
			t.Errorf("validUUID(%q) = %v, want %v", id, got, want)
		}
	}
}
