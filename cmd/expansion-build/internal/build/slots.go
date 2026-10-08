package build

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/puppet-stagehand/stagehand-sdk/manifest"
	"github.com/puppet-stagehand/stagehand-sdk/uibundle"
)

// readSlots reads <src>/slots.json and checks it against manifest.slots.
func readSlots(src string, pm *manifest.Manifest) (map[string]uibundle.SlotMeta, []manifest.Finding, error) {
	raw, err := os.ReadFile(filepath.Join(src, "slots.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, []manifest.Finding{{Code: "ui_slots_json_missing", Path: "slots.json",
				Message: "ui/src/slots.json is required",
				Fix:     `Create slots.json mapping each slot in manifest.slots to its tab label, for example {"nodeDetailTab": "Hello"}.`}}, nil
		}
		return nil, nil, err
	}
	var labels map[string]string
	if err := json.Unmarshal(raw, &labels); err != nil {
		return nil, []manifest.Finding{{Code: "ui_slots_json_invalid", Path: "slots.json", Message: err.Error(),
			Fix: `Make slots.json one JSON object of slot name to label string.`}}, nil
	}
	var fs []manifest.Finding
	declared := map[string]bool{}
	for _, s := range pm.Slots {
		declared[s] = true
		if _, ok := labels[s]; !ok {
			fs = append(fs, manifest.Finding{Code: "ui_slot_missing", Path: "slots.json#" + s,
				Message: "manifest.slots declares " + s + " but slots.json has no label for it",
				Fix:     "Add a label for " + s + " to slots.json, or remove the slot from manifest.slots."})
		}
	}
	meta := map[string]uibundle.SlotMeta{}
	for name, label := range labels {
		if !declared[name] {
			fs = append(fs, manifest.Finding{Code: "ui_slot_not_declared", Path: "slots.json#" + name,
				Message: "slots.json labels " + name + " but manifest.slots does not declare it",
				Fix:     "Declare the slot in manifest.slots, or remove it from slots.json."})
			continue
		}
		if msg := uibundle.LabelProblem(label); msg != "" {
			fs = append(fs, manifest.Finding{Code: "ui_slot_label_invalid", Path: "slots.json#" + name, Message: msg,
				Fix: "Use 1 to 32 characters of plain text (no control characters, < or >)."})
		}
		meta[name] = uibundle.SlotMeta{Label: label}
	}
	return meta, fs, nil
}
