// Package uibundle is the contract between a pack's built UI and the console
// (UI bundle contract, format 1).
//
// A pack image carries /stagehand/ui/ui.manifest.json. It is the bundle's index:
// which file is the entry module, what label each slot gets, and the sha256,
// size and content type of every file. manifest.ui_digest is
// "sha256:" + hex(sha256(exact bytes of ui.manifest.json)), so the digest covers
// the index and the index covers every file. The console fetches the index,
// checks the digest, then fetches and hash-checks only the files it lists.
//
// This package is the reference implementation of those rules. pack-check
// uses it for `--ui`; the console implements the same rules independently
// and the two agree because the digest is over exact bytes (no canonical form).
package uibundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/puppet-stagehand/stagehand-sdk/manifest"
)

const (
	// ManifestPath is the bundle index, relative to /stagehand/ui/.
	ManifestPath = "ui.manifest.json"
	// FormatVersion is the only ui.manifest.json format this SDK writes.
	FormatVersion = 1
	// ContractVersion is the version of the default-export entry shape
	// (`{ contract_version: 1, slots }`) and of the /runtime/v1/ shim URLs.
	ContractVersion = 1

	MaxManifestBytes = 256 << 10
	MaxFileBytes     = 8 << 20
	MaxBundleBytes   = 16 << 20
	MaxFiles         = 256
	MaxPathChars     = 200
	MaxLabelChars    = 32
	// MaxAssetChunkBytes is the largest Assets.Get chunk a worker may send.
	MaxAssetChunkBytes = 1 << 20
)

// ContentTypes are the only types a bundle file may declare. SVG is excluded
// on purpose: it can carry script.
var ContentTypes = []string{
	"text/javascript", "text/css", "application/json",
	"image/png", "image/webp", "font/woff2",
}

// Shims maps each module a pack build must treat as external to the
// same-origin URL it is rewritten to. Adding an export behind a shim is
// additive; removing one is a contract break.
var Shims = map[string]string{
	"react":                 "/runtime/v1/react.js",
	"react/jsx-runtime":     "/runtime/v1/jsx-runtime.js",
	"react-dom":             "/runtime/v1/react-dom.js",
	"@stagehand/console-ui": "/runtime/v1/console-ui.js",
}

// ConsoleUIPrimitives are the exports of /runtime/v1/console-ui.js.
var ConsoleUIPrimitives = []string{
	"Alert", "Badge", "Button", "Card", "Checkbox", "Collapsible", "Dialog",
	"Drawer", "HelpBubble", "Input", "LoadingState", "Radio", "SectionHeader",
	"Select", "Spinner", "StatBlock", "Switch", "Tabs", "Tag", "Tooltip",
}

// File describes one bundle file.
type File struct {
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

// SlotMeta is the host-rendered metadata for a slot.
type SlotMeta struct {
	Label string `json:"label"`
}

// Manifest is ui.manifest.json.
type Manifest struct {
	Format       int                 `json:"format"`
	Entry        string              `json:"entry"`
	SandboxEntry string              `json:"sandbox_entry,omitempty"`
	Slots        map[string]SlotMeta `json:"slots"`
	Files        map[string]File     `json:"files"`
}

var (
	rePathChars = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	reSHA256    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// DigestOf returns the ui_digest for the exact bytes of a ui.manifest.json.
func DigestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidPath reports whether p is a legal bundle path: relative, at most 200
// characters of [A-Za-z0-9._/-], no "." or ".." or empty segments, no leading
// slash, no backslash, no NUL.
func ValidPath(p string) bool {
	if p == "" || len(p) > MaxPathChars || !rePathChars.MatchString(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func contentTypeAllowed(ct string) bool {
	for _, c := range ContentTypes {
		if c == ct {
			return true
		}
	}
	return false
}

// Parse decodes ui.manifest.json strictly and applies every rule that needs
// only the file itself. Unknown fields are findings.
func Parse(raw []byte) (*Manifest, []manifest.Finding) {
	var fs []manifest.Finding
	add := func(code, path, msg, fix string) {
		fs = append(fs, manifest.Finding{Code: code, Path: path, Message: msg, Fix: fix})
	}
	if len(raw) > MaxManifestBytes {
		add("ui_manifest_too_large", "/", fmt.Sprintf("ui.manifest.json is %d bytes; the limit is %d", len(raw), MaxManifestBytes),
			"Drop files from the bundle or shorten paths.")
		return nil, fs
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		add("ui_manifest_unparseable", "/", err.Error(),
			"Make ui.manifest.json valid JSON with only format, entry, sandbox_entry, slots and files.")
		return nil, fs
	}
	if dec.More() {
		add("ui_manifest_unparseable", "/", "trailing data after the JSON object", "ui.manifest.json must hold exactly one JSON object.")
		return nil, fs
	}
	if m.Format != FormatVersion {
		add("ui_format_unsupported", "/format", fmt.Sprintf("format must be %d", FormatVersion), "Set \"format\": 1.")
	}
	if len(m.Files) == 0 {
		add("ui_files_empty", "/files", "files must list at least the entry module", "List every file the bundle serves under files.")
	}
	if len(m.Files) > MaxFiles {
		add("ui_too_many_files", "/files", fmt.Sprintf("%d files; the limit is %d", len(m.Files), MaxFiles), "Bundle more aggressively.")
	}

	paths := make([]string, 0, len(m.Files))
	for p := range m.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var total int64
	for _, p := range paths {
		f := m.Files[p]
		at := "/files/" + p
		if !ValidPath(p) {
			add("ui_path_invalid", at, "illegal bundle path", fmt.Sprintf("Use a relative path of at most %d characters from [A-Za-z0-9._/-], with no \".\", \"..\", empty segment, leading slash or backslash.", MaxPathChars))
		}
		if !reSHA256.MatchString(f.SHA256) {
			add("ui_file_sha256_invalid", at+"/sha256", "sha256 must be 64 lowercase hex characters", "Write the lowercase hex sha256 of the file bytes.")
		}
		if f.Size < 0 || f.Size > MaxFileBytes {
			add("ui_file_too_large", at+"/size", fmt.Sprintf("size %d is outside 0..%d", f.Size, MaxFileBytes), "Split or shrink the file.")
		}
		total += f.Size
		if !contentTypeAllowed(f.ContentType) {
			add("ui_content_type_not_allowed", at+"/content_type", fmt.Sprintf("content type %q is not allowed", f.ContentType),
				"Use one of: "+strings.Join(ContentTypes, ", ")+". SVG is excluded because it can carry script.")
		}
	}
	if total > MaxBundleBytes {
		add("ui_bundle_too_large", "/files", fmt.Sprintf("bundle is %d bytes; the limit is %d", total, MaxBundleBytes), "Shrink the bundle.")
	}

	checkModule := func(field, p string, required bool) {
		if p == "" {
			if required {
				add("ui_entry_missing", "/"+field, field+" is required", "Set "+field+" to the path of the ES module listed in files.")
			}
			return
		}
		f, ok := m.Files[p]
		if !ok {
			add("ui_entry_not_listed", "/"+field, field+" "+p+" is not listed in files", "Add the file to files, or point "+field+" at a listed file.")
			return
		}
		if f.ContentType != "text/javascript" {
			add("ui_entry_not_javascript", "/"+field, field+" must have content_type text/javascript", "Set content_type to text/javascript for the entry module.")
		}
	}
	checkModule("entry", m.Entry, true)
	checkModule("sandbox_entry", m.SandboxEntry, false)

	if len(m.Slots) == 0 {
		add("ui_slots_empty", "/slots", "slots must name at least one slot", "Add an entry per slot the bundle provides.")
	}
	names := make([]string, 0, len(m.Slots))
	for n := range m.Slots {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		at := "/slots/" + n
		known := false
		for _, s := range manifest.Slots {
			if s == n {
				known = true
			}
		}
		if !known {
			add("ui_slot_unknown", at, "unknown slot "+n, "Use one of: "+strings.Join(manifest.Slots, ", ")+".")
		}
		if msg := LabelProblem(m.Slots[n].Label); msg != "" {
			add("ui_slot_label_invalid", at+"/label", msg, fmt.Sprintf("Use 1–%d characters of plain text (no control characters, < or >).", MaxLabelChars))
		}
	}
	return &m, fs
}

// LabelProblem returns why l is not a valid slot label, or the empty string.
func LabelProblem(l string) string {
	if !utf8.ValidString(l) {
		return "label is not valid UTF-8"
	}
	n := utf8.RuneCountInString(l)
	if n < 1 || n > MaxLabelChars {
		return fmt.Sprintf("label has %d characters; it must have 1–%d", n, MaxLabelChars)
	}
	if strings.TrimSpace(l) != l || l == "" {
		return "label must not start or end with whitespace"
	}
	for _, r := range l {
		if unicode.IsControl(r) || r == '<' || r == '>' {
			return "label must be plain text"
		}
	}
	return ""
}

// Verify checks a built UI directory (the contents of /stagehand/ui/) against
// the pack manifest. It reports every finding it can: the index itself, the
// digest, each listed file's presence, size and sha256, files present but not
// listed, and agreement between the index's slots and manifest.slots.
func Verify(dir string, pm *manifest.Manifest) []manifest.Finding {
	var fs []manifest.Finding
	add := func(code, path, msg, fix string) {
		fs = append(fs, manifest.Finding{Code: code, Path: path, Message: msg, Fix: fix})
	}
	raw, err := os.ReadFile(filepath.Join(dir, ManifestPath))
	if err != nil {
		add("ui_manifest_missing", "/"+ManifestPath, err.Error(), "Run the pack build; it writes ui.manifest.json into the ui directory.")
		return fs
	}
	um, pf := Parse(raw)
	fs = append(fs, pf...)
	if pm != nil {
		if got := DigestOf(raw); pm.UIDigest != got {
			add("ui_digest_mismatch", "/ui_digest", fmt.Sprintf("manifest ui_digest is %q but ui.manifest.json hashes to %s", pm.UIDigest, got),
				"Rebuild the pack; the build writes ui_digest. Never hand-edit it.")
		}
	}
	if um == nil {
		return fs
	}
	listed := map[string]bool{ManifestPath: true}
	paths := make([]string, 0, len(um.Files))
	for p := range um.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if !ValidPath(p) {
			continue // already a ui_path_invalid finding; never touch the disk with it
		}
		listed[p] = true
		f := um.Files[p]
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			add("ui_file_missing", "/files/"+p, err.Error(), "Rebuild the pack so every listed file is in the ui directory.")
			continue
		}
		if int64(len(body)) != f.Size {
			add("ui_file_size_mismatch", "/files/"+p+"/size", fmt.Sprintf("listed %d bytes, file has %d", f.Size, len(body)), "Rebuild the pack; the index is generated.")
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			add("ui_file_hash_mismatch", "/files/"+p+"/sha256", "file bytes do not match the listed sha256", "Rebuild the pack; the index is generated.")
		}
	}
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !listed[rel] {
			add("ui_file_unlisted", "/files/"+rel, "file is in the ui directory but not listed in ui.manifest.json; the console never serves it",
				"List it in files or remove it from the bundle.")
		}
		return nil
	})
	if pm != nil {
		declared := map[string]bool{}
		for _, s := range pm.Slots {
			declared[s] = true
			if _, ok := um.Slots[s]; !ok {
				add("ui_slot_missing", "/slots/"+s, "manifest.slots declares "+s+" but ui.manifest.json has no label for it", "Add the slot to ui.manifest.json slots, or remove it from manifest.slots.")
			}
		}
		for s := range um.Slots {
			if !declared[s] {
				add("ui_slot_not_declared", "/slots/"+s, "ui.manifest.json provides "+s+" but manifest.slots does not declare it", "Declare the slot in manifest.slots, or remove it from the bundle.")
			}
		}
	}
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].Path < fs[j].Path })
	return fs
}
