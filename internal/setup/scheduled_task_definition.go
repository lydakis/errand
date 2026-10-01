package setup

import (
	"encoding/xml"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Export-ScheduledTask emits UTF-8 through our PowerShell query, although its
// XML declaration still names UTF-16. The saved file really is UTF-16.
func taskXMLDecoder(text string) *xml.Decoder {
	decoder := xml.NewDecoder(strings.NewReader(text))
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		if strings.EqualFold(charset, "utf-16") {
			return input, nil
		}
		return nil, fmt.Errorf("unexpected task XML encoding %q", charset)
	}
	return decoder
}

func setupRegisteredTask(definition, command, user, configPath, logPath, stateDir string) bool {
	if !retainedTaskCommand(command, stateDir) {
		return false
	}
	return sameTaskDefinition(definition, renderScheduledTaskText(user, command, configPath, logPath))
}

type taskXMLNode struct {
	XMLName    xml.Name
	Attributes []xml.Attr    `xml:",any,attr"`
	Children   []taskXMLNode `xml:",any"`
	Text       string        `xml:",chardata"`
}

// Task Scheduler reorders XML and inserts schema defaults when registering a
// task. Compare all other fields, including unknown ones, rather than matching
// a subset that could discard an operator's settings or extra actions.
func sameTaskDefinition(actual, expected string) bool {
	a, err := taskDefinitionFields(actual)
	if err != nil {
		return false
	}
	e, err := taskDefinitionFields(expected)
	return err == nil && slices.Equal(a, e)
}

var taskSchemaDefaults = map[string]string{
	"Task/Settings/WakeToRun":                       "false",
	"Task/Settings/UseUnifiedSchedulingEngine":      "false",
	"Task/Settings/DisallowStartOnRemoteAppSession": "false",
	"Task/Settings/Volatile":                        "false",
	"Task/Settings/IdleSettings/Duration":           "PT10M",
	"Task/Settings/IdleSettings/WaitTimeout":        "PT1H",
	"Task/Triggers/LogonTrigger/Delay":              "PT0S",
	"Task/Actions/Exec/WorkingDirectory":            "",
	"Task/Actions/@Context":                         "Author",
	"Task/RegistrationInfo/SecurityDescriptor":      "",
}

func taskDefinitionFields(text string) ([]string, error) {
	var root taskXMLNode
	if err := taskXMLDecoder(text).Decode(&root); err != nil {
		return nil, err
	}
	if root.XMLName.Local != "Task" {
		return nil, fmt.Errorf("task XML has no Task root")
	}
	var fields []string
	var visit func(taskXMLNode, string)
	visit = func(node taskXMLNode, parent string) {
		path := node.XMLName.Local
		if parent != "" {
			path = parent + "/" + path
		}
		// These registration timestamps and authors are scheduler metadata.
		if path == "Task/RegistrationInfo/Date" || path == "Task/RegistrationInfo/Author" {
			return
		}
		for _, attr := range node.Attributes {
			if attr.Name.Local == "xmlns" || attr.Name.Space == "xmlns" || (path == "Task" && attr.Name.Local == "version") {
				continue // the scheduler updates the XML schema version
			}
			key := path + "/@" + attr.Name.Local
			if value, ok := taskSchemaDefaults[key]; !ok || attr.Value != value {
				fields = append(fields, key+"="+attr.Value)
			}
		}
		if len(node.Children) == 0 {
			if value, ok := taskSchemaDefaults[path]; !ok || node.Text != value {
				text := node.Text
				if path == "Task/Principals/Principal/UserId" || path == "Task/Triggers/LogonTrigger/UserId" {
					text = taskUserID(text)
				}
				fields = append(fields, path+"="+text)
			}
			return
		}
		for _, child := range node.Children {
			visit(child, path)
		}
	}
	visit(root, "")
	slices.Sort(fields)
	return fields, nil
}
