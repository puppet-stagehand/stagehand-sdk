package code

// Compile-only stubs for the RED commit; the implementation follows.

import (
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

func ParseHierarchyLenient(yamlText string, lim ImportLimits) (*hostv1.HieraHierarchy, string, []*hostv1.ImportFinding, bool) {
	return nil, "", nil, false
}

func ParseDataFileLenient(path, yamlText string, lim ImportLimits) (*hostv1.ImportDataFile, []*hostv1.ImportFinding) {
	return nil, nil
}
