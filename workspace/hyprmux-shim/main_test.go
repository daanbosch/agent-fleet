package main

import "testing"

func TestConfigFromEnvRequiresEveryKey(t *testing.T) {
	full := map[string]string{
		"HYPRMUX_SHIM_ADDR":  "127.0.0.1:18500",
		"HYPRMUX_SHIM_TOKEN": "tok",
		"HYPRMUX_REGISTRY":   "/x/sessions-v4.json",
		"HYPRMUX_AGENT_LOGS": "/x/agent-logs",
	}
	get := func(env map[string]string) func(string) string {
		return func(k string) string { return env[k] }
	}

	cfg, addr, err := configFromEnv(get(full))
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if addr != "127.0.0.1:18500" || cfg.Token != "tok" || cfg.RegistryPath != "/x/sessions-v4.json" || cfg.LogsDir != "/x/agent-logs" {
		t.Fatalf("cfg = %+v addr = %q", cfg, addr)
	}

	for _, k := range []string{"HYPRMUX_SHIM_ADDR", "HYPRMUX_SHIM_TOKEN", "HYPRMUX_REGISTRY", "HYPRMUX_AGENT_LOGS"} {
		missing := map[string]string{}
		for kk, v := range full {
			if kk != k {
				missing[kk] = v
			}
		}
		if _, _, err := configFromEnv(get(missing)); err == nil {
			t.Errorf("without %s: want error, got nil", k)
		}
	}

	exposed := map[string]string{}
	for kk, v := range full {
		exposed[kk] = v
	}
	exposed["HYPRMUX_SHIM_ADDR"] = "0.0.0.0:18500"
	if _, _, err := configFromEnv(get(exposed)); err == nil {
		t.Error("non-loopback bind: want error, got nil")
	}
}
