// Package agentinstructions discovers and bounds AGENTS.md-compatible workspace
// instructions, preserving the DSH-compatible durable provenance expected by
// the Web context surface.
package agentinstructions

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

const (
	// DefaultMaxBytes is the rendered UTF-8 budget for one instruction baseline.
	DefaultMaxBytes = 65_536
	// DefaultMaxSourceBytes is the read cap for one instruction source.
	DefaultMaxSourceBytes = 1_048_576
	userGlobalDirectory   = "user-global"
	systemReminderOpen    = "<system-reminder>"
	systemReminderClose   = "</system-reminder>"
	scopeSeparator        = "\x00"
)

var (
	defaultCandidates      = []string{"AGENTS.md", "CLAUDE.md"}
	defaultLocalCandidates = []string{"AGENTS.local.md", "CLAUDE.local.md"}
	defaultRootMarkers     = []string{".git"}
)

// Config controls discovery and bounded rendering.
type Config struct {
	Home                           string
	ProjectRootMarkers             []string
	MaxBytes                       int
	MaxSourceBytes                 int
	InstructionFileCandidates      []string
	LocalInstructionFileCandidates []string
}

// File is one loaded instruction source in model precedence order.
type File struct {
	AbsolutePath string
	DisplayPath  string
	Content      string
}

// Change is one durable source transition represented by the context.
type Change struct {
	Action string `json:"action"`
	Scope  string `json:"scope"`
	Path   string `json:"path"`
	Digest string `json:"digest,omitempty"`
}

// State is the active instruction state visible to the model. It contains only
// identities and paths, never instruction prose.
type State struct {
	Identity string
	Changes  map[string]Change
}

func (s *State) clone() *State {
	if s == nil {
		return nil
	}
	out := &State{Identity: s.Identity, Changes: make(map[string]Change, len(s.Changes))}
	for scope, change := range s.Changes {
		out.Changes[scope] = change
	}
	return out
}

// Message renders a baseline as a durable user-role context message.
func Message(cwd string, config Config) (*llm.Message, error) {
	message, _, err := Reconcile(cwd, config, nil)
	return message, err
}

// Reconcile compares current files with the caller's visible instruction
// state. A nil or identity-changing previous state publishes a complete
// baseline; the same identity publishes only set/replace/remove deltas.
func Reconcile(cwd string, config Config, previous *State) (*llm.Message, *State, error) {
	return ReconcileTouch(cwd, config, previous, nil)
}

// ReconcileTouch adds the descendant-directory scopes crossed by successful
// file-tool touches. The touched paths mirror DSH's read/write/edit projection:
// they discover nested instruction files below the session cwd, while a touch
// at the cwd root itself does not widen the baseline ancestor chain.
func ReconcileTouch(cwd string, config Config, previous *State, touchedPaths []string) (*llm.Message, *State, error) {
	resolved := config.resolve()
	files, identity, err := LoadReconciliationFiles(cwd, config, previous, touchedPaths)
	if err != nil {
		return nil, nil, err
	}
	text, represented := RenderBaseline(files, resolved.MaxBytes, false)
	current := make([]Change, 0, len(represented))
	for _, file := range represented {
		current = append(current, Change{
			Action: "set", Scope: scopeKeyForFile(file), Path: file.DisplayPath,
			Digest: ContentDigest(file.Content),
		})
	}
	if previous == nil || previous.Identity != identity {
		replace := previous != nil
		if replace {
			text, represented = RenderBaseline(files, resolved.MaxBytes, true)
		}
		if text == "" {
			return nil, previous.clone(), nil
		}
		activeChanges := append([]Change(nil), current...)
		changes := current
		if replace {
			replaced := make(map[string]bool, len(current))
			for _, change := range current {
				replaced[change.Scope] = true
			}
			changes = make([]Change, 0, len(previous.Changes)+len(current))
			for scope, before := range previous.Changes {
				if !replaced[scope] {
					changes = append(changes, Change{Action: "remove", Scope: scope, Path: before.Path})
				}
			}
			changes = append(changes, current...)
		}
		next := &State{Identity: identity, Changes: changesByID(activeChanges)}
		return baselineMessage(text, identity, changes), next, nil
	}

	changeItems := make([]changeRenderItem, 0, len(represented))
	currentByID := make(map[string]Change, len(represented))
	absoluteCWD, _ := filepath.Abs(cwd)
	root := FindProjectRoot(absoluteCWD, resolved.ProjectRootMarkers)
	preserved := unavailablePreviousGroups(previous, resolved, root, pathUnavailable)
	for index, file := range represented {
		change := current[index]
		scope := change.Scope
		currentByID[scope] = change
		before, visible := previous.Changes[scope]
		if visible && preserved[scope] {
			// An unavailable sibling makes the whole same-directory candidate
			// group unobservable. Keep every previously visible member instead
			// of interpreting a transient probe failure as file removal.
			currentByID[scope] = before
			continue
		}
		if visible && before.Path == change.Path && before.Digest == change.Digest {
			continue
		}
		if visible {
			change.Action = "replace"
		}
		changeItems = append(changeItems, changeRenderItem{change: change, file: file})
	}
	for scope, before := range previous.Changes {
		if preserved[scope] {
			currentByID[scope] = before
			continue
		}
		if _, active := currentByID[scope]; active {
			continue
		}
		changeItems = append(changeItems, changeRenderItem{
			change: Change{Action: "remove", Scope: scope, Path: before.Path},
			file:   File{AbsolutePath: "removed:" + scope, DisplayPath: before.Path},
		})
	}
	sort.Slice(changeItems, func(left, right int) bool {
		if changeItems[left].change.Action != changeItems[right].change.Action {
			return changeItems[left].change.Action < changeItems[right].change.Action
		}
		return changeItems[left].change.Scope < changeItems[right].change.Scope
	})
	if len(changeItems) == 0 {
		return nil, previous.clone(), nil
	}
	text, representedChanges := renderChangeItems(changeItems, resolved.MaxBytes)
	if text == "" || len(representedChanges) == 0 {
		return nil, previous.clone(), nil
	}
	next := previous.clone()
	next.Identity = identity
	for _, change := range representedChanges {
		if change.Action == "remove" {
			delete(next.Changes, change.Scope)
			continue
		}
		next.Changes[change.Scope] = change
	}
	return deltaMessage(text, representedChanges), next, nil
}

// unavailablePreviousGroups identifies active same-directory groups whose
// candidate set cannot be fully observed. The returned map is keyed by scope so
// reconciliation can preserve each previous member unchanged.
func unavailablePreviousGroups(
	previous *State, resolved Config, root string, unavailable func(string) bool,
) map[string]bool {
	if previous == nil || len(previous.Changes) == 0 {
		return nil
	}
	byDirectory := make(map[string][]string)
	for scope := range previous.Changes {
		directory, _, ok := strings.Cut(scope, scopeSeparator)
		if ok && directory != "" && directory != userGlobalDirectory {
			byDirectory[directory] = append(byDirectory[directory], scope)
		}
	}
	preserved := make(map[string]bool)
	for directory, scopes := range byDirectory {
		dir := root
		if directory != "." {
			dir = filepath.Join(root, filepath.FromSlash(directory))
		}
		groupUnavailable := false
		for _, group := range [][]string{resolved.InstructionFileCandidates, resolved.LocalInstructionFileCandidates} {
			for _, candidate := range group {
				if unavailable(filepath.Join(dir, candidate)) {
					groupUnavailable = true
					break
				}
			}
		}
		if groupUnavailable {
			for _, scope := range scopes {
				preserved[scope] = true
			}
		}
	}
	return preserved
}

func pathUnavailable(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return !os.IsNotExist(err)
	}
	return false
}

func baselineMessage(text, identity string, changes []Change) *llm.Message {
	return &llm.Message{
		Role:                   llm.RoleUser,
		Content:                []llm.ContentBlock{llm.Text(text)},
		SourceKind:             "agent-instructions",
		SourceForm:             "instructions",
		SourceBaseline:         true,
		SourceBaselineIdentity: identity,
		SourceChanges:          changes,
	}
}

func deltaMessage(text string, changes []Change) *llm.Message {
	return &llm.Message{
		Role:          llm.RoleUser,
		Content:       []llm.ContentBlock{llm.Text(text)},
		SourceKind:    "agent-instructions",
		SourceForm:    "instructions",
		SourceChanges: changes,
	}
}

func changesByID(changes []Change) map[string]Change {
	out := make(map[string]Change, len(changes))
	for _, change := range changes {
		out[change.Scope] = change
	}
	return out
}

// LoadBaseline returns discovered and read files in broadest-to-specific order.
// A nil result means no readable instruction source was found.
func LoadBaseline(cwd string, config Config) ([]File, string, error) {
	return LoadReconciliationFiles(cwd, config, nil, nil)
}

// LoadReconciliationFiles discovers the baseline ancestor chain plus the
// previously-active and newly-touched scopes required for one reconciliation.
func LoadReconciliationFiles(cwd string, config Config, previous *State, touchedPaths []string) ([]File, string, error) {
	resolved := config.resolve()
	if resolved.MaxBytes <= 0 || resolved.MaxSourceBytes <= 0 {
		return nil, "", nil
	}
	absoluteCWD, err := filepath.Abs(cwd)
	if err != nil {
		return nil, "", err
	}
	absoluteCWD = filepath.Clean(absoluteCWD)
	root := FindProjectRoot(absoluteCWD, resolved.ProjectRootMarkers)
	identityPayload, err := json.Marshal(baselineIdentity{
		ProjectRoot:                    relativeIdentityPath(absoluteCWD, root),
		ProjectRootMarkers:             resolved.ProjectRootMarkers,
		MaxBytes:                       resolved.MaxBytes,
		MaxSourceBytes:                 resolved.MaxSourceBytes,
		InstructionFileCandidates:      resolved.InstructionFileCandidates,
		LocalInstructionFileCandidates: resolved.LocalInstructionFileCandidates,
	})
	if err != nil {
		return nil, "", err
	}
	identity := string(identityPayload)

	var files []File
	if value, ok, err := readFile(filepath.Join(resolved.Home, "AGENTS.md"), "$SHUTU_HOME/AGENTS.md", resolved.MaxSourceBytes); err != nil {
		return nil, "", err
	} else if ok {
		files = append(files, value)
	}
	seen := map[string]bool{}
	for _, dir := range ancestorChain(root, absoluteCWD) {
		for _, group := range [][]string{resolved.InstructionFileCandidates, resolved.LocalInstructionFileCandidates} {
			for _, name := range group {
				path := filepath.Join(dir, name)
				if seen[path] {
					continue
				}
				seen[path] = true
				value, ok, err := readFile(path, relativeDisplay(root, path), resolved.MaxSourceBytes)
				if err != nil {
					return nil, "", err
				}
				if ok {
					files = append(files, value)
				}
			}
		}
	}
	for _, scope := range sortedScopes(previous) {
		directory, candidate, ok := strings.Cut(scope, scopeSeparator)
		if !ok || directory == "" || candidate == "" || directory == userGlobalDirectory {
			continue
		}
		dir := absoluteCWD
		if directory != "." {
			dir = filepath.Join(root, filepath.FromSlash(directory))
		}
		path := filepath.Join(dir, candidate)
		if seen[path] {
			continue
		}
		seen[path] = true
		value, ok, err := readFile(path, relativeDisplay(root, path), resolved.MaxSourceBytes)
		if err != nil {
			return nil, "", err
		}
		if ok {
			files = append(files, value)
		}
	}
	if err := appendTouchedFiles(root, absoluteCWD, resolved, touchedPaths, seen, &files); err != nil {
		return nil, "", err
	}
	files = dedupeByDirectory(files)
	if len(files) == 0 {
		return nil, identity, nil
	}
	return files, identity, nil
}

type baselineIdentity struct {
	ProjectRoot                    string   `json:"projectRoot"`
	ProjectRootMarkers             []string `json:"projectRootMarkers"`
	MaxBytes                       int      `json:"maxBytes"`
	MaxSourceBytes                 int      `json:"maxSourceBytes"`
	InstructionFileCandidates      []string `json:"instructionFileCandidates"`
	LocalInstructionFileCandidates []string `json:"localInstructionFileCandidates"`
}

func relativeIdentityPath(from, to string) string {
	relative, err := filepath.Rel(from, to)
	if err != nil {
		return to
	}
	return relative
}

func sortedScopes(state *State) []string {
	if state == nil || len(state.Changes) == 0 {
		return nil
	}
	scopes := make([]string, 0, len(state.Changes))
	for scope := range state.Changes {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	return scopes
}

func appendTouchedFiles(root, cwd string, resolved Config, touchedPaths []string, seen map[string]bool, files *[]File) error {
	if len(touchedPaths) == 0 {
		return nil
	}
	ordered := append([]string(nil), touchedPaths...)
	sort.Strings(ordered)
	visited := make(map[string]bool)
	for _, touched := range ordered {
		if strings.TrimSpace(touched) == "" {
			continue
		}
		target := touched
		if !filepath.IsAbs(target) {
			target = filepath.Join(cwd, target)
		}
		target = filepath.Clean(target)
		targetDir := filepath.Dir(target)
		relative, err := filepath.Rel(cwd, targetDir)
		if err != nil || relative == "." || strings.HasPrefix(relative, "..") || filepath.IsAbs(relative) {
			continue
		}
		for _, dir := range ancestorChain(cwd, targetDir)[1:] {
			if visited[dir] {
				continue
			}
			visited[dir] = true
			for _, group := range [][]string{resolved.InstructionFileCandidates, resolved.LocalInstructionFileCandidates} {
				for _, name := range group {
					path := filepath.Join(dir, name)
					if seen[path] {
						continue
					}
					seen[path] = true
					value, ok, err := readFile(path, relativeDisplay(root, path), resolved.MaxSourceBytes)
					if err != nil {
						return err
					}
					if ok {
						*files = append(*files, value)
					}
				}
			}
		}
	}
	return nil
}

// FindProjectRoot walks upward to the first directory containing a marker.
func FindProjectRoot(cwd string, markers []string) string {
	if len(markers) == 0 {
		markers = defaultRootMarkers
	}
	current := filepath.Clean(cwd)
	for {
		for _, marker := range markers {
			if info, err := os.Stat(filepath.Join(current, marker)); err == nil {
				_ = info
				return current
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(cwd)
		}
		current = parent
	}
}

func (c Config) resolve() Config {
	home := strings.TrimSpace(c.Home)
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil && userHome != "" {
			home = filepath.Join(userHome, ".shutu")
		}
	}
	resolved := Config{
		Home:                           home,
		ProjectRootMarkers:             defaultRootMarkers,
		MaxBytes:                       DefaultMaxBytes,
		MaxSourceBytes:                 DefaultMaxSourceBytes,
		InstructionFileCandidates:      defaultCandidates,
		LocalInstructionFileCandidates: defaultLocalCandidates,
	}
	if len(c.ProjectRootMarkers) > 0 {
		resolved.ProjectRootMarkers = c.ProjectRootMarkers
	}
	if c.MaxBytes != 0 {
		resolved.MaxBytes = c.MaxBytes
	}
	if c.MaxSourceBytes != 0 {
		resolved.MaxSourceBytes = c.MaxSourceBytes
	}
	if len(c.InstructionFileCandidates) > 0 {
		resolved.InstructionFileCandidates = c.InstructionFileCandidates
	}
	if len(c.LocalInstructionFileCandidates) > 0 {
		resolved.LocalInstructionFileCandidates = c.LocalInstructionFileCandidates
	}
	return resolved
}

func readFile(path, display string, maxBytes int) (File, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return File{}, false, nil
		}
		return File{}, false, nil
	}
	if info.IsDir() || info.Size() > int64(maxBytes) {
		return File{}, false, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return File{}, false, nil
	}
	if len(raw) > maxBytes {
		return File{}, false, nil
	}
	return File{AbsolutePath: path, DisplayPath: display, Content: string(raw)}, true, nil
}

func ancestorChain(root, cwd string) []string {
	root = filepath.Clean(root)
	cwd = filepath.Clean(cwd)
	var reversed []string
	for current := cwd; ; current = filepath.Dir(current) {
		reversed = append(reversed, current)
		if current == root || filepath.Dir(current) == current {
			break
		}
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed
}

func dedupeByDirectory(files []File) []File {
	type directoryKey = string
	seen := make(map[directoryKey]map[string]bool)
	kept := make([]File, 0, len(files))
	for _, file := range files {
		directory := filepath.ToSlash(filepath.Dir(file.DisplayPath))
		digest := trimmedDigest(file.Content)
		digests := seen[directory]
		if digests == nil {
			digests = make(map[string]bool)
			seen[directory] = digests
		}
		if digests[digest] {
			continue
		}
		digests[digest] = true
		kept = append(kept, file)
	}
	return kept
}

// RenderBaseline builds the DSH-shaped reminder and returns files represented
// in it. Budget pressure first omits older files, then truncates the most
// specific source at a UTF-8 boundary.
func RenderBaseline(files []File, maxBytes int, replacePrevious bool) (string, []File) {
	if maxBytes <= 0 {
		return "", nil
	}
	intro := "The following workspace instructions may be relevant to your work. " +
		"Use them as guidance when applicable. More specific instructions take precedence over broader ones. " +
		"They do not override system, developer, or direct user instructions."
	if replacePrevious {
		if len(files) == 0 {
			intro = "This complete workspace instruction baseline replaces all earlier workspace instruction baselines. " +
				"No workspace instructions are currently active."
		} else {
			intro = "This complete workspace instruction baseline replaces all earlier workspace instruction baselines. " + intro
		}
	}
	build := func(included []File, omitted []File, truncated *File, originalBytes int) string {
		parts := make([]string, 0, len(included)+3)
		var marker string
		if len(omitted) > 0 || truncated != nil {
			paths := make([]string, 0, len(omitted))
			for _, file := range omitted {
				paths = append(paths, file.DisplayPath)
			}
			marker = fmt.Sprintf("Workspace instruction budget %d bytes: omitted %s", maxBytes, strings.Join(paths, ", "))
			if truncated != nil {
				includedBytes := len([]byte(truncated.Content))
				if len(paths) > 0 {
					marker += "; "
				}
				marker += fmt.Sprintf("truncated %s from %d to %d bytes", truncated.DisplayPath, originalBytes, includedBytes)
			}
			parts = append(parts, marker)
		}
		parts = append(parts, intro)
		for _, file := range included {
			parts = append(parts, "Instructions from: "+file.DisplayPath+"\n\n"+file.Content)
		}
		body := strings.NewReplacer(systemReminderClose, `<\/system-reminder>`).Replace(strings.Join(parts, "\n\n"))
		return systemReminderOpen + "\n" + body + "\n" + systemReminderClose
	}

	full := build(files, nil, nil, 0)
	if len(full) <= maxBytes {
		return full, files
	}
	for start := 1; start < len(files); start++ {
		omitted := files[:start]
		candidate := build(files[start:], omitted, nil, 0)
		if len(candidate) <= maxBytes {
			return candidate, files[start:]
		}
	}
	if len(files) == 0 {
		return "", nil
	}
	mostSpecific := files[len(files)-1]
	omitted := files[:len(files)-1]
	originalBytes := len([]byte(mostSpecific.Content))
	low, high := 0, originalBytes
	bestContent := ""
	for low <= high {
		mid := (low + high) / 2
		candidateContent := truncateUTF8(mostSpecific.Content, mid)
		candidateFile := mostSpecific
		candidateFile.Content = candidateContent
		candidate := build([]File{candidateFile}, omitted, &candidateFile, originalBytes)
		if len(candidate) <= maxBytes {
			bestContent = candidateContent
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	truncatedFile := mostSpecific
	truncatedFile.Content = bestContent
	rendered := build([]File{truncatedFile}, omitted, &truncatedFile, originalBytes)
	if len(rendered) > maxBytes {
		return "", nil
	}
	return rendered, []File{truncatedFile}
}

// ScopeForFile returns the DSH-compatible logical directory scope.
func ScopeForFile(file File) string {
	if file.DisplayPath == "$SHUTU_HOME/AGENTS.md" {
		return userGlobalDirectory
	}
	return filepath.ToSlash(filepath.Dir(file.DisplayPath))
}

func scopeKeyForFile(file File) string {
	return CandidateScope(ScopeForFile(file), filepath.Base(file.DisplayPath))
}

// changeRenderItem pairs one durable transition with the file content used to
// render its semantic section. Removals use an empty placeholder file.
type changeRenderItem struct {
	change Change
	file   File
}

type truncatedChange struct {
	path          string
	originalBytes int
	includedBytes int
}

func changeSection(item changeRenderItem) string {
	switch item.change.Action {
	case "remove":
		return "Instructions removed: " + item.change.Path + "\n\n" +
			"The previously loaded instructions from this file no longer apply."
	case "replace":
		return "Updated instructions from: " + item.file.DisplayPath + "\n\n" +
			"This file changed after it was loaded. Use the following content instead of the previously loaded instructions from this file.\n\n" +
			item.file.Content
	default:
		scope := ScopeForFile(item.file)
		return "Additional instructions from: " + item.file.DisplayPath + "\n\n" +
			"These instructions apply to work under `" + scope +
			"`. Use them as guidance when relevant; more specific instructions take precedence. They do not override system, developer, or direct user instructions.\n\n" +
			item.file.Content
	}
}

func buildChangeText(
	items []changeRenderItem,
	maxBytes int,
	omitted []changeRenderItem,
	truncated *truncatedChange,
) string {
	var marker string
	if len(omitted) > 0 || truncated != nil {
		paths := make([]string, 0, len(omitted))
		for _, item := range omitted {
			paths = append(paths, item.change.Path)
		}
		marker = fmt.Sprintf("Workspace instruction budget %d bytes: omitted %s", maxBytes, strings.Join(paths, ", "))
		if truncated != nil {
			if len(paths) > 0 {
				marker += "; "
			}
			marker += fmt.Sprintf("truncated %s from %d to %d bytes", truncated.path, truncated.originalBytes, truncated.includedBytes)
		}
	}
	parts := make([]string, 0, len(items)+2)
	if marker != "" {
		parts = append(parts, marker)
	}
	for _, item := range items {
		parts = append(parts, changeSection(item))
	}
	body := strings.NewReplacer(systemReminderClose, `<\/system-reminder>`).Replace(strings.Join(parts, "\n\n"))
	return systemReminderOpen + "\n" + body + "\n" + systemReminderClose
}

func truncateChangeToFit(
	item changeRenderItem,
	maxBytes int,
	omitted []changeRenderItem,
) (changeRenderItem, truncatedChange, bool) {
	originalBytes := len(item.file.Content)
	low, high := 0, originalBytes
	best := item
	best.file.Content = ""
	for low <= high {
		mid := (low + high) / 2
		candidate := item
		candidate.file.Content = truncateUTF8(item.file.Content, mid)
		truncated := truncatedChange{
			path: item.change.Path, originalBytes: originalBytes,
			includedBytes: len(candidate.file.Content),
		}
		if len(buildChangeText([]changeRenderItem{candidate}, maxBytes, omitted, &truncated)) <= maxBytes {
			best = candidate
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	truncated := truncatedChange{
		path: item.change.Path, originalBytes: originalBytes,
		includedBytes: len(best.file.Content),
	}
	return best, truncated, true
}

// renderChangeItems builds a bounded DSH-shaped reconciliation notice and
// returns only transitions semantically represented by the final text.
func renderChangeItems(items []changeRenderItem, maxBytes int) (string, []Change) {
	if maxBytes <= 0 {
		return "", nil
	}
	full := buildChangeText(items, maxBytes, nil, nil)
	if len(full) <= maxBytes {
		changes := make([]Change, 0, len(items))
		for _, item := range items {
			changes = append(changes, item.change)
		}
		return full, changes
	}
	for start := 1; start < len(items); start++ {
		omitted := append([]changeRenderItem(nil), items[:start]...)
		suffix := items[start:]
		text := buildChangeText(suffix, maxBytes, omitted, nil)
		if len(text) <= maxBytes {
			changes := make([]Change, 0, len(suffix))
			for _, item := range suffix {
				changes = append(changes, item.change)
			}
			return text, changes
		}
	}
	mostSpecific := items[len(items)-1]
	omitted := append([]changeRenderItem(nil), items[:len(items)-1]...)
	truncatedItem, truncated, _ := truncateChangeToFit(mostSpecific, maxBytes, omitted)
	text := buildChangeText([]changeRenderItem{truncatedItem}, maxBytes, omitted, &truncated)
	if len(text) <= maxBytes {
		if len(truncatedItem.file.Content) == 0 && len(mostSpecific.file.Content) != 0 {
			return text, nil
		}
		return text, []Change{mostSpecific.change}
	}

	compact := truncatedChange{
		path:          mostSpecific.change.Path,
		originalBytes: len(mostSpecific.file.Content), includedBytes: 0,
	}
	notice := fmt.Sprintf("Workspace instruction budget %d bytes: truncated %s from %d to 0 bytes", maxBytes, mostSpecific.change.Path, len(mostSpecific.file.Content))
	if len(systemReminderOpen+"\n"+strings.NewReplacer(systemReminderClose, `<\/system-reminder>`).Replace(notice)+"\n"+systemReminderClose) <= maxBytes {
		return buildChangeText(nil, maxBytes, nil, &compact), nil
	}
	return truncateUTF8(notice, maxBytes), nil
}

func CandidateScope(directory, candidate string) string {
	return directory + scopeSeparator + candidate
}

func ContentDigest(content string) string {
	sum := sha1.Sum([]byte(content))
	return hex.EncodeToString(sum[:])
}

func trimmedDigest(content string) string {
	return ContentDigest(strings.TrimSpace(content))
}

func relativeDisplay(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(relative)
}

func relativeOrDot(root, path string) string {
	value := relativeDisplay(root, path)
	if value == "" {
		return "."
	}
	return value
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes >= len(value) {
		return value
	}
	if maxBytes <= 0 {
		return ""
	}
	for maxBytes > 0 && (value[maxBytes]&0xc0) == 0x80 {
		maxBytes--
	}
	return value[:maxBytes]
}

// SortedCopy is a test helper that makes expected path comparisons explicit.
func SortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
