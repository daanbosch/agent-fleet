package fastvm

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultSecurityTemplateID = 9013
const defaultSecurityFeatures = "crowdstrike,lacework,auditd,fluent-bit"

func securityTemplateIDs() map[int]bool {
	raw := strings.TrimSpace(os.Getenv("FASTVM_SECURITY_TEMPLATE_IDS"))
	if raw == "" {
		raw = strconv.Itoa(defaultSecurityTemplateID)
	}
	out := map[int]bool{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		id, err := strconv.Atoi(item)
		if err != nil || id <= 0 {
			continue
		}
		out[id] = true
	}
	return out
}

func isSecurityTemplateID(template int) bool {
	if template <= 0 {
		return false
	}
	return securityTemplateIDs()[template]
}

func (s *StateManager) isRegisteredSecurityTemplate(template string) bool {
	if template == "" {
		return false
	}
	if s == nil {
		return false
	}
	if tpl, ok := s.GetTemplate(template); ok {
		return tpl.SecurityTemplate
	}
	return false
}

func proxmoxTemplateHasSecurityBaseline(state *StateManager, template int) bool {
	if isSecurityTemplateID(template) {
		return true
	}
	if state == nil {
		return false
	}
	return state.isRegisteredSecurityTemplate(strconv.Itoa(template))
}

func requireProxmoxSnapshotSecurityBaseline(state *StateManager, vmid string, vm *VMState) error {
	if vm == nil {
		return fmt.Errorf("VM %s not found", vmid)
	}
	if vm.SecurityTemplate {
		return nil
	}
	if proxmoxTemplateHasSecurityBaseline(state, vm.Template) {
		return nil
	}
	return fmt.Errorf("VM %s cannot be snapshotted into a reusable template because its source template is not marked as a FastVM security template; create customization VMs from template %d or another registered secure template", vmid, defaultSecurityTemplateID)
}

func firecrackerTemplateNameFromVM(vm *VMState) string {
	if vm == nil {
		return ""
	}
	if vm.TemplateName != "" {
		return vm.TemplateName
	}
	if vm.Template > 0 {
		return strconv.Itoa(vm.Template)
	}
	return ""
}

func firecrackerTemplateOrRegistryHasSecurityBaseline(state *StateManager, installDir, template string) bool {
	if template == "" {
		return false
	}
	if id, err := strconv.Atoi(template); err == nil && isSecurityTemplateID(id) {
		return true
	}
	if firecrackerTemplateHasSecurityBaseline(installDir, template) {
		return true
	}
	return state.isRegisteredSecurityTemplate(template)
}

func requireFirecrackerSnapshotSecurityBaseline(state *StateManager, installDir, vmid string, vm *VMState) error {
	if vm == nil {
		return fmt.Errorf("VM %s not found", vmid)
	}
	if vm.SecurityTemplate {
		return nil
	}
	sourceTemplate := firecrackerTemplateNameFromVM(vm)
	if firecrackerTemplateOrRegistryHasSecurityBaseline(state, installDir, sourceTemplate) {
		return nil
	}
	return fmt.Errorf("VM %s cannot be snapshotted into a reusable template because its source template is not marked as a FastVM security template; create customization VMs from template %d or another registered secure template", vmid, defaultSecurityTemplateID)
}

func firecrackerTemplateEnvPath(installDir, template string) (string, error) {
	template = strings.TrimSpace(template)
	if template == "" {
		return "", fmt.Errorf("template name is empty")
	}
	if strings.Contains(template, "/") || strings.Contains(template, "\\") || template == "." || template == ".." {
		return "", fmt.Errorf("unsafe template name %q", template)
	}
	return filepath.Join(installDir, "templates", template+".env"), nil
}

func readTemplateEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func firecrackerTemplateHasSecurityBaseline(installDir, template string) bool {
	path, err := firecrackerTemplateEnvPath(installDir, template)
	if err != nil {
		return false
	}
	values, err := readTemplateEnv(path)
	if err != nil {
		return false
	}
	return values["SECURITY_TEMPLATE"] == "1"
}

func inheritFirecrackerSecurityMetadata(installDir, sourceTemplate, targetTemplate, sourceVMID string) error {
	features := firecrackerSecurityFeatures(installDir, sourceTemplate)

	targetPath, err := firecrackerTemplateEnvPath(installDir, targetTemplate)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(targetPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open target template metadata: %w", err)
	}
	defer f.Close()

	if _, err := fmt.Fprintf(f, "SECURITY_TEMPLATE=1\nSECURITY_FEATURES=%s\nSECURITY_INHERITED_FROM=%s\nSECURITY_INHERITED_SOURCE_VMID=%s\nSECURITY_INHERITED_AT=%s\n",
		features,
		sourceTemplate,
		sourceVMID,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("write target template metadata: %w", err)
	}
	return nil
}

func firecrackerSecurityFeatures(installDir, template string) string {
	path, err := firecrackerTemplateEnvPath(installDir, template)
	if err != nil {
		return defaultSecurityFeatures
	}
	values, err := readTemplateEnv(path)
	if err != nil {
		return defaultSecurityFeatures
	}
	if strings.TrimSpace(values["SECURITY_FEATURES"]) == "" {
		return defaultSecurityFeatures
	}
	return values["SECURITY_FEATURES"]
}
