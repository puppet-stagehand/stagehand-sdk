package code

// RED-phase stub (13-02 Task 1): the real validator lands in the GREEN commit.

// ModuledirMaxLen is the longest accepted moduledir value.
const ModuledirMaxLen = 64

// ModuledirError is returned by ValidateModuledir for an unsafe value.
type ModuledirError struct {
	Value, Reason, Fix string
}

func (e *ModuledirError) Error() string { return "" }

// ValidateModuledir is a stub that accepts everything.
func ValidateModuledir(s string) error { return nil }
