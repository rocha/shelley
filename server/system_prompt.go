package server

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"shelley.exe.dev/skills"
)

//go:embed system_prompt.txt
var systemPromptTemplate string

//go:embed subagent_system_prompt.txt
var subagentSystemPromptTemplate string

// SystemPromptData contains all the data needed to render the system prompt template
type SystemPromptData struct {
	WorkingDirectory string
	GitInfo          *GitInfo
	Codebase         *CodebaseInfo
	IsExeDev         bool
	IsSudoAvailable  bool
	Hostname         string // For exe.dev, the public hostname (e.g., "vmname.exe.xyz")
	ShelleyDBPath    string // Path to the shelley database
	SkillsXML        string // XML block for available skills
	UserEmail        string // The exe.dev auth email of the user, if known
}

// DBPath is the path to the shelley database, set at startup
var DBPath string

type GitInfo struct {
	Root string
}

type CodebaseInfo struct {
	InjectFiles         []string
	InjectFileContents  map[string]string
	SubdirGuidanceFiles []string
}

// SubdirGuidanceSummary returns a prompt-friendly summary of subdirectory guidance files.
// If ≤10, lists them explicitly. If >10, lists the first 10 and notes how many more exist.
func (c *CodebaseInfo) SubdirGuidanceSummary() string {
	if len(c.SubdirGuidanceFiles) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nSubdirectory guidance files (read before editing files in these directories):\n")
	show := c.SubdirGuidanceFiles
	if len(show) > 10 {
		show = show[:10]
	}
	for _, f := range show {
		b.WriteString(f)
		b.WriteByte('\n')
	}
	if len(c.SubdirGuidanceFiles) > 10 {
		fmt.Fprintf(&b, "...and %d more. Use `find` to discover others.\n", len(c.SubdirGuidanceFiles)-10)
	}
	return b.String()
}

// SystemPromptOption configures optional fields on the system prompt.
type SystemPromptOption func(*SystemPromptData)

// WithUserEmail sets the user's email in the system prompt.
func WithUserEmail(email string) SystemPromptOption {
	return func(d *SystemPromptData) {
		d.UserEmail = email
	}
}

// GenerateSystemPrompt generates the system prompt using the embedded template.
// If workingDir is empty, it uses the current working directory.
func GenerateSystemPrompt(workingDir string, opts ...SystemPromptOption) (string, error) {
	data, err := collectSystemData(workingDir)
	if err != nil {
		return "", fmt.Errorf("failed to collect system data: %w", err)
	}

	for _, opt := range opts {
		opt(data)
	}

	tmpl, err := template.New("system_prompt").Parse(systemPromptTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	var buf strings.Builder
	err = tmpl.Execute(&buf, data)
	if err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return collapseBlankLines(buf.String()), nil
}

// collapseBlankLines reduces runs of 3+ newlines to 2 (one blank line)
// and trims leading/trailing whitespace.
var reBlankRun = regexp.MustCompile(`\n{3,}`)

func collapseBlankLines(s string) string {
	s = strings.TrimSpace(s)
	s = reBlankRun.ReplaceAllString(s, "\n\n")
	return s + "\n"
}

func collectSystemData(workingDir string) (*SystemPromptData, error) {
	wd := workingDir
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to get working directory: %w", err)
		}
	}

	data := &SystemPromptData{
		WorkingDirectory: wd,
	}

	// Try to collect git info
	gitInfo, err := collectGitInfo(wd)
	if err == nil {
		data.GitInfo = gitInfo
	}

	// Collect codebase info
	codebaseInfo, err := collectCodebaseInfo(wd, gitInfo)
	if err == nil {
		data.Codebase = codebaseInfo
	}

	// Check if running on exe.dev
	data.IsExeDev = isExeDev()

	// Check sudo availability
	data.IsSudoAvailable = isSudoAvailable()

	// Get hostname for exe.dev
	if data.IsExeDev {
		if hostname, err := os.Hostname(); err == nil {
			// If hostname doesn't contain dots, add .exe.xyz suffix
			if !strings.Contains(hostname, ".") {
				hostname = hostname + ".exe.xyz"
			}
			data.Hostname = hostname
		}
	}

	// Set shelley database path if it was configured
	if DBPath != "" {
		// Convert to absolute path if relative
		if !filepath.IsAbs(DBPath) {
			if absPath, err := filepath.Abs(DBPath); err == nil {
				data.ShelleyDBPath = absPath
			} else {
				data.ShelleyDBPath = DBPath
			}
		} else {
			data.ShelleyDBPath = DBPath
		}
	}

	// Discover and load skills
	var gitRoot string
	if gitInfo != nil {
		gitRoot = gitInfo.Root
	}
	data.SkillsXML = collectSkills(wd, gitRoot)

	return data, nil
}

func collectGitInfo(dir string) (*GitInfo, error) {
	// Find git root
	rootCmd := exec.Command("git", "rev-parse", "--show-toplevel")
	if dir != "" {
		rootCmd.Dir = dir
	}
	rootOutput, err := rootCmd.Output()
	if err != nil {
		return nil, err
	}
	root := strings.TrimSpace(string(rootOutput))

	return &GitInfo{
		Root: root,
	}, nil
}

// atFileRe matches @path tokens preceded by whitespace, an opening delimiter, or
// start-of-line. This excludes email addresses (e.g. user@host has no leading
// whitespace before the @). Square bracket [ is intentionally excluded: [@file](url)
// is a Markdown hyperlink and should not be treated as a file reference.
var atFileRe = regexp.MustCompile(`(?:^|[\s({<])@(\S+)`)

// stripInlineCode replaces the contents of backtick spans with spaces so that
// @refs inside inline code are not extracted. An unclosed span extends to end of line.
func stripInlineCode(line string) string {
	var b strings.Builder
	inCode := false
	for i := 0; i < len(line); i++ {
		if line[i] == '`' {
			inCode = !inCode
			b.WriteByte(' ')
		} else if inCode {
			b.WriteByte(' ')
		} else {
			b.WriteByte(line[i])
		}
	}
	return b.String()
}

// resolveAtPath resolves a raw @-reference token to an absolute path.
// Returns "" if the token is not a recognisable path form (e.g. starts with @ or #).
func resolveAtPath(raw, fromFile string) string {
	switch {
	case strings.HasPrefix(raw, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Clean(filepath.Join(home, raw[2:]))
	case filepath.IsAbs(raw):
		return filepath.Clean(raw)
	case len(raw) > 0 && isAtPathStart(raw[0]):
		return filepath.Clean(filepath.Join(filepath.Dir(fromFile), raw))
	default:
		return ""
	}
}

// isAtPathStart returns true for characters that may begin a valid path token:
// word characters, dot (for ./rel and bare names), slash, or tilde.
func isAtPathStart(c byte) bool {
	return c == '.' || c == '/' || c == '~' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '_' || c == '-'
}

// atPathsInContent extracts the absolute paths of all @file references in content.
// It skips fenced code blocks (``` or ~~~) and inline backtick spans so that @refs
// inside code are not followed. fromFile is the absolute path of the containing file
// and is used to resolve relative references.
func atPathsInContent(content, fromFile string) []string {
	var paths []string
	inFence := false
	for _, line := range strings.Split(content, "\n") {
		// Track fenced code block boundaries (``` or ~~~).
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for _, m := range atFileRe.FindAllStringSubmatch(stripInlineCode(line), -1) {
			raw := m[1]
			// Strip trailing punctuation unlikely to be part of a filename.
			raw = strings.TrimRight(raw, ".,):!?")
			// Strip URL-style fragment suffix.
			if i := strings.Index(raw, "#"); i != -1 {
				raw = raw[:i]
			}
			if p := resolveAtPath(raw, fromFile); p != "" {
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// addGuidanceFile loads the file at path into info if it has not been seen before,
// then follows any @file references in its content. seen (keyed on lowercased path,
// matching the existing convention in collectCodebaseInfo) prevents loading the same
// file twice and is the sole cycle-detection mechanism — no depth limit is needed.
func addGuidanceFile(path string, info *CodebaseInfo, seen map[string]bool) {
	key := strings.ToLower(path)
	if seen[key] {
		return
	}
	content, err := os.ReadFile(path)
	// Mark seen immediately after reading, even if content is empty, so repeated
	// references to the same missing or empty file do not cause redundant reads.
	seen[key] = true
	if err != nil || len(content) == 0 {
		return
	}
	info.InjectFiles = append(info.InjectFiles, path)
	info.InjectFileContents[path] = string(content)
	// Follow @file references found in this file. The shared seen map ensures
	// that cycles (e.g. a child referencing its parent) terminate naturally.
	for _, ref := range atPathsInContent(string(content), path) {
		addGuidanceFile(ref, info, seen)
	}
}

func collectCodebaseInfo(wd string, gitInfo *GitInfo) (*CodebaseInfo, error) {
	info := &CodebaseInfo{
		InjectFiles:        []string{},
		InjectFileContents: make(map[string]string),
	}

	// Track seen files to avoid duplicates on case-insensitive file systems.
	// The same map is reused for @file reference expansion in addGuidanceFile.
	seenFiles := make(map[string]bool)

	// Check for user-level agent instructions in ~/.config/AGENTS.md, ~/.config/shelley/AGENTS.md, and ~/.shelley/AGENTS.md
	if home, err := os.UserHomeDir(); err == nil {
		userAgentsFiles := []string{
			filepath.Join(home, ".config", "AGENTS.md"),
			filepath.Join(home, ".config", "shelley", "AGENTS.md"),
			filepath.Join(home, ".shelley", "AGENTS.md"),
		}
		for _, f := range userAgentsFiles {
			addGuidanceFile(f, info, seenFiles)
		}
	}

	// Determine the root directory to search
	searchRoot := wd
	if gitInfo != nil {
		searchRoot = gitInfo.Root
	}

	// Find root-level guidance files (case-insensitive)
	for _, file := range findGuidanceFilesInDir(searchRoot) {
		addGuidanceFile(file, info, seenFiles)
	}

	// If working directory is different from root, also check working directory
	if wd != searchRoot {
		for _, file := range findGuidanceFilesInDir(wd) {
			addGuidanceFile(file, info, seenFiles)
		}
	}

	// Find subdirectory guidance files for the system prompt listing
	info.SubdirGuidanceFiles = findSubdirGuidanceFiles(searchRoot)

	return info, nil
}

func findGuidanceFilesInDir(dir string) []string {
	// Read directory entries to handle case-insensitive file systems
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var found []string
	seen := make(map[string]bool)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		lowerName := strings.ToLower(entry.Name())
		if isGuidanceFile(lowerName) && lowerName != "readme.md" && !seen[lowerName] {
			seen[lowerName] = true
			found = append(found, filepath.Join(dir, entry.Name()))
		}
	}
	return found
}

// isGuidanceFile returns true if the lowercased filename is a recognized guidance file.
func isGuidanceFile(lowerName string) bool {
	switch lowerName {
	case "agents.md", "agent.md", "claude.md", "dear_llm.md", "readme.md":
		return true
	}
	return false
}

// findSubdirGuidanceFiles returns guidance files in subdirectories of root (not root itself).
func findSubdirGuidanceFiles(root string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var found []string
	seen := make(map[string]bool)

	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil {
			return nil // Continue on errors
		}
		if info.IsDir() {
			// Skip hidden directories and common ignore patterns
			if strings.HasPrefix(info.Name(), ".") || info.Name() == "node_modules" || info.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		// Only count files in subdirectories, not root
		if filepath.Dir(path) != root && isGuidanceFile(strings.ToLower(info.Name())) {
			lowerPath := strings.ToLower(path)
			if !seen[lowerPath] {
				seen[lowerPath] = true
				found = append(found, path)
			}
		}
		return nil
	})
	return found
}

func isExeDev() bool {
	_, err := os.Stat("/exe.dev")
	return err == nil
}

// collectSkills discovers skills from default directories, project .skills dirs,
// and the project tree.
func collectSkills(workingDir, gitRoot string) string {
	// Start with default directories (user-level skills)
	dirs := skills.DefaultDirs()

	// Add .skills directories found in the project tree
	dirs = append(dirs, skills.ProjectSkillsDirs(workingDir, gitRoot)...)

	// Discover skills from all directories
	foundSkills := skills.Discover(dirs)

	// Also discover skills anywhere in the project tree
	treeSkills := skills.DiscoverInTree(workingDir, gitRoot)

	// Merge, avoiding duplicates by path
	seen := make(map[string]bool)
	for _, s := range foundSkills {
		seen[s.Path] = true
	}
	for _, s := range treeSkills {
		if !seen[s.Path] {
			foundSkills = append(foundSkills, s)
			seen[s.Path] = true
		}
	}

	// Generate XML
	return skills.ToPromptXML(foundSkills)
}

func isSudoAvailable() bool {
	cmd := exec.Command("sudo", "-n", "id")
	_, err := cmd.CombinedOutput()
	return err == nil
}

// SubagentSystemPromptData contains data for subagent system prompts (minimal subset)
type SubagentSystemPromptData struct {
	WorkingDirectory string
	GitInfo          *GitInfo
}

// GenerateSubagentSystemPrompt generates a minimal system prompt for subagent conversations.
func GenerateSubagentSystemPrompt(workingDir string) (string, error) {
	wd := workingDir
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get working directory: %w", err)
		}
	}

	data := &SubagentSystemPromptData{
		WorkingDirectory: wd,
	}

	// Try to collect git info
	gitInfo, err := collectGitInfo(wd)
	if err == nil {
		data.GitInfo = gitInfo
	}

	tmpl, err := template.New("subagent_system_prompt").Parse(subagentSystemPromptTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse subagent template: %w", err)
	}

	var buf strings.Builder
	err = tmpl.Execute(&buf, data)
	if err != nil {
		return "", fmt.Errorf("failed to execute subagent template: %w", err)
	}

	return collapseBlankLines(buf.String()), nil
}
