package setup

import (
	"strings"
	"testing"
)

func TestTaskDefinitionPreservesOperatorSettings(t *testing.T) {
	expected, err := decodeUTF16(renderScheduledTask("S-1-5-21-1001", `C:\runtime\errand.exe`, `C:\config.toml`, `C:\log.txt`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, actual string
		same         bool
	}{
		{name: "unchanged", actual: expected, same: true},
		{name: "scheduler-version-and-context", actual: strings.ReplaceAll(strings.ReplaceAll(expected,
			`version="1.2"`, `version="1.3"`), `<Actions Context="Author">`, `<Actions>`), same: true},
		{name: "empty-security-descriptor", actual: strings.ReplaceAll(expected, "<RegistrationInfo>", "<RegistrationInfo><SecurityDescriptor/>"), same: true},
		{name: "scheduler-defaults", actual: strings.ReplaceAll(strings.ReplaceAll(expected,
			"<Settings>", "<Settings><WakeToRun>false</WakeToRun><UseUnifiedSchedulingEngine>false</UseUnifiedSchedulingEngine>"),
			"<IdleSettings>", "<IdleSettings><Duration>PT10M</Duration><WaitTimeout>PT1H</WaitTimeout>"), same: true},
		{name: "scheduler-metadata", actual: strings.ReplaceAll(expected, "<RegistrationInfo>", "<RegistrationInfo><Date>2026-10-01T00:00:00</Date><Author>user</Author>"), same: true},
		{name: "element-order", actual: strings.ReplaceAll(expected,
			"<Enabled>true</Enabled>\r\n      <UserId>S-1-5-21-1001</UserId>",
			"<UserId>S-1-5-21-1001</UserId>\r\n      <Enabled>true</Enabled>"), same: true},
		{name: "arguments", actual: strings.ReplaceAll(expected, "serve --config", "serve --other --config")},
		{name: "priority", actual: strings.ReplaceAll(expected, "<Priority>4</Priority>", "<Priority>7</Priority>")},
		{name: "changed-default", actual: strings.ReplaceAll(expected, "<Settings>", "<Settings><WakeToRun>true</WakeToRun>")},
		{name: "working-directory", actual: strings.ReplaceAll(expected, "</Exec>", `<WorkingDirectory>C:\tools</WorkingDirectory></Exec>`)},
		{name: "extra-action", actual: strings.ReplaceAll(expected, "</Actions>", `<Exec><Command>C:\tools\wrapper.exe</Command></Exec></Actions>`)},
		{name: "extra-trigger", actual: strings.ReplaceAll(expected, "</Triggers>", "<BootTrigger/></Triggers>")},
		{name: "unknown-setting", actual: strings.ReplaceAll(expected, "<Settings>", "<Settings><UnknownSetting/>")},
		{name: "security-descriptor", actual: strings.ReplaceAll(expected, "<RegistrationInfo>", "<RegistrationInfo><SecurityDescriptor>D:(A;;FA;;;SY)</SecurityDescriptor>")},
		{name: "action-context", actual: strings.ReplaceAll(expected, `Context="Author"`, `Context="Other"`)},
		{name: "principal-user", actual: strings.ReplaceAll(expected, "<UserId>S-1-5-21-1001</UserId>", "<UserId>S-1-5-21-1002</UserId>")},
		{name: "malformed", actual: "not XML"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameTaskDefinition(tc.actual, expected); got != tc.same {
				t.Fatalf("sameTaskDefinition = %v, want %v", got, tc.same)
			}
		})
	}
}
