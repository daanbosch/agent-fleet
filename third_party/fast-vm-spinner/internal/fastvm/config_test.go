package fastvm

import "testing"

func TestDefaultTemplateID(t *testing.T) {
	t.Setenv("FASTVM_DEFAULT_TEMPLATE", "9013")

	if got := defaultTemplateID(); got != 9013 {
		t.Fatalf("defaultTemplateID() = %d, want 9013", got)
	}
}

func TestDefaultTemplateIDFallback(t *testing.T) {
	t.Setenv("FASTVM_DEFAULT_TEMPLATE", "not-a-vmid")

	if got := defaultTemplateID(); got != fallbackTemplate {
		t.Fatalf("defaultTemplateID() = %d, want %d", got, fallbackTemplate)
	}
}

func TestIsBaseTemplate(t *testing.T) {
	if !isBaseTemplate(9013, 9013) {
		t.Fatal("configured default template should be treated as a base template")
	}
	if !isBaseTemplate(fallbackTemplate, 9013) {
		t.Fatal("fallback template should remain a base template when default is overridden")
	}
	if isBaseTemplate(60000, 9013) {
		t.Fatal("user snapshot template should not be treated as a base template")
	}
}

func TestProxmoxNICQueues(t *testing.T) {
	cases := []struct {
		name  string
		cores int
		want  int
	}{
		{name: "default for unset", cores: 0, want: defaultCores},
		{name: "matches small VM", cores: 16, want: 16},
		{name: "allows proxmox max", cores: 64, want: 64},
		{name: "caps large VM", cores: 120, want: proxmoxMaxNICQueues},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxmoxNICQueues(tc.cores); got != tc.want {
				t.Fatalf("proxmoxNICQueues(%d) = %d, want %d", tc.cores, got, tc.want)
			}
		})
	}
}
