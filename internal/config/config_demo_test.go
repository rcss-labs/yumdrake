package config

import (
	"strings"
	"testing"
)

func TestRefusesDemoKeyOutsideDev(t *testing.T) {
	loaders := map[string]func(map[string]string) error{
		"orchestrator": func(env map[string]string) error {
			_, err := LoadOrchestrator(envFrom(env))
			return err
		},
		"runner": func(env map[string]string) error {
			_, err := LoadRunner(envFrom(env))
			return err
		},
	}
	cases := []struct {
		name    string
		key     string
		env     string
		wantErr bool
	}{
		{"demo key in prod is refused", DemoSecretKey, "prod", true},
		{"demo key with no env (default prod) is refused", DemoSecretKey, "", true},
		{"demo key in dev is allowed", DemoSecretKey, "dev", false},
		{"another key in prod is allowed", validSecretKey, "prod", false},
	}
	for loaderName, load := range loaders {
		for _, tc := range cases {
			t.Run(loaderName+"/"+tc.name, func(t *testing.T) {
				env := requiredEnv()
				env["YUMDRAKE_PUBLIC_URL"] = "http://localhost:18280"
				env["YUMDRAKE_LOCAL_SECRET_KEY"] = tc.key
				if tc.env != "" {
					env["YUMDRAKE_ENV"] = tc.env
				}
				err := load(env)
				if tc.wantErr {
					if err == nil || !strings.Contains(err.Error(), "YUMDRAKE_LOCAL_SECRET_KEY") {
						t.Fatalf("error = %v, want one naming YUMDRAKE_LOCAL_SECRET_KEY", err)
					}
					if strings.Contains(err.Error(), DemoSecretKey) {
						t.Errorf("error echoes the key: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("error = %v, want none", err)
				}
			})
		}
	}
}

func TestRefusesDevWithPublicHostname(t *testing.T) {
	cases := []struct {
		name      string
		env       string
		publicURL string
		wantErr   bool
	}{
		{"dev with a public hostname is refused", "dev", "https://yumdrake.example.com", true},
		{"dev on localhost is allowed", "dev", "http://localhost:18280", false},
		{"dev on 127.0.0.1 is allowed", "dev", "http://127.0.0.1:18280", false},
		{"prod with a public hostname is allowed", "prod", "https://yumdrake.example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := requiredEnv()
			env["YUMDRAKE_ENV"] = tc.env
			env["YUMDRAKE_PUBLIC_URL"] = tc.publicURL
			_, err := LoadOrchestrator(envFrom(env))
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "YUMDRAKE_ENV") {
					t.Fatalf("error = %v, want one naming YUMDRAKE_ENV", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want none", err)
			}
		})
	}
}
