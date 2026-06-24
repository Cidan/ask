package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"
)

const agentReadToolDescription = `Read a file from the filesystem. Returns the content with 1-based line numbers (cat -n format). Use offset/limit for large files; lines longer than 2000 chars are truncated. Reading a file is required before editing or overwriting it.`

type agentReadParams struct {
	FilePath    string `json:"file_path" description:"absolute or cwd-relative path of the file to read"`
	Offset      int    `json:"offset,omitempty" description:"1-based line number to start reading from (default 1)"`
	Limit       int    `json:"limit,omitempty" description:"maximum number of lines to return (default 2000)"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

func agentReadTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"read",
		agentReadToolDescription,
		func(ctx context.Context, p agentReadParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			path := env.absPath(p.FilePath)
			info, err := os.Stat(path)
			if err != nil {
				if os.IsNotExist(err) {
					return fantasy.NewTextErrorResponse("file not found: " + path), nil
				}
				return fantasy.NewTextErrorResponse("stat " + path + ": " + err.Error()), nil
			}
			if info.IsDir() {
				return fantasy.NewTextErrorResponse(path + " is a directory; use the ls tool instead"), nil
			}
			if agentImageExts[strings.ToLower(filepath.Ext(path))] {
				return fantasy.NewTextErrorResponse("image files are not supported: the deepseek models cannot accept image input"), nil
			}

			f, err := os.Open(path)
			if err != nil {
				return fantasy.NewTextErrorResponse("open " + path + ": " + err.Error()), nil
			}
			defer f.Close()

			head := make([]byte, 8192)
			n, _ := f.Read(head)
			if looksBinary(head[:n]) {
				return fantasy.NewTextErrorResponse(path + " looks like a binary file; reading it would not be useful"), nil
			}
			if _, err := f.Seek(0, 0); err != nil {
				return fantasy.NewTextErrorResponse("seek " + path + ": " + err.Error()), nil
			}

			offset := max(p.Offset, 1)
			limit := p.Limit
			if limit <= 0 {
				limit = agentMaxReadLines
			}

			var out strings.Builder
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
			lineNo := 0
			emitted := 0
			truncatedBytes := false
			moreLines := false
			for sc.Scan() {
				lineNo++
				if lineNo < offset {
					continue
				}
				if emitted >= limit {
					moreLines = true
					break
				}
				fmt.Fprintf(&out, "%6d\t%s\n", lineNo, truncateLine(sc.Text()))
				emitted++
				if out.Len() >= agentMaxReadBytes {
					truncatedBytes = true
					break
				}
			}
			if err := sc.Err(); err != nil {
				return fantasy.NewTextErrorResponse("read " + path + ": " + err.Error()), nil
			}

			env.files.recordRead(path)
			if emitted == 0 {
				if offset > 1 {
					return fantasy.NewTextResponse(fmt.Sprintf("(no lines at offset %d; file has %d lines)", offset, lineNo)), nil
				}
				return fantasy.NewTextResponse("(empty file)"), nil
			}
			body := out.String()
			switch {
			case truncatedBytes:
				body += fmt.Sprintf("(output capped at %d bytes; continue with offset %d)\n", agentMaxReadBytes, offset+emitted)
			case moreLines:
				body += fmt.Sprintf("(file has more lines; continue with offset %d)\n", offset+emitted)
			}
			return fantasy.NewTextResponse(body), nil
		},
	)
}

const agentWriteToolDescription = `Create or overwrite a file with the given content. Overwriting an existing file requires reading it first in this session. Parent directories are created automatically.`

type agentWriteParams struct {
	FilePath    string `json:"file_path" description:"absolute or cwd-relative path of the file to write"`
	Content     string `json:"content" description:"the full new content of the file"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

func agentWriteTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"write",
		agentWriteToolDescription,
		func(ctx context.Context, p agentWriteParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if env.planningMode {
				return fantasy.NewTextResponse("Planning mode is ON. File modifications are currently blocked."), nil
			}
			if strings.TrimSpace(p.FilePath) == "" {
				return fantasy.NewTextErrorResponse("file_path is required"), nil
			}
			path := env.absPath(p.FilePath)
			// When gateTodosBeforeMutate is on, no mutation before a task
			// list exists: writing is code-change intent, and the todos call
			// is the mandatory chokepoint where workflows are checked.
			// Refuse until todos has applied.
			//
			// Workflow plan files live under ask/plans/ and must be writable
			// before a workflow_run can be submitted (the runner gates on the
			// start plan existing). Allowing them through here breaks the
			// circular dependency between "write the plan" and "create a task
			// list / run a workflow".
			if !isPathUnderWorkflowPlans(env.cwd, path) {
				if notice := env.requireTodosNotice(); notice != "" {
					return fantasy.NewTextResponse(notice), nil
				}
			}
			oldContent := ""
			mode := os.FileMode(0o644)
			if info, err := os.Stat(path); err == nil {
				if info.IsDir() {
					return fantasy.NewTextErrorResponse(path + " is a directory"), nil
				}
				if guard := env.checkReadBeforeMutate(path, info.ModTime()); guard != "" {
					return fantasy.NewTextErrorResponse(guard), nil
				}
				mode = info.Mode().Perm()
				data, err := os.ReadFile(path)
				if err != nil {
					return fantasy.NewTextErrorResponse("read " + path + ": " + err.Error()), nil
				}
				oldContent = string(data)
				if oldContent == p.Content {
					return fantasy.NewTextResponse("no change: " + path + " already has that exact content"), nil
				}
			}

			if denied := env.requestApproval(ctx, "write", map[string]any{
				"file_path":   path,
				"content":     p.Content,
				"description": p.Description,
			}); denied != nil {
				return *denied, nil
			}

			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fantasy.NewTextErrorResponse("mkdir " + filepath.Dir(path) + ": " + err.Error()), nil
			}
			if err := os.WriteFile(path, []byte(p.Content), mode); err != nil {
				return fantasy.NewTextErrorResponse("write " + path + ": " + err.Error()), nil
			}
			env.files.recordRead(path)
			env.emitFileDiff(path, oldContent, p.Content)
			if oldContent == "" {
				return fantasy.NewTextResponse("created " + path), nil
			}
			return fantasy.NewTextResponse("updated " + path), nil
		},
	)
}

const agentEditToolDescription = `Replace an exact string in a file. old_string must match the file content exactly, including whitespace and indentation, and must be unique in the file unless replace_all is set. Use an empty old_string to create a new file. The file must have been read in this session before editing.`

type agentEditParams struct {
	FilePath    string `json:"file_path" description:"absolute or cwd-relative path of the file to edit"`
	OldString   string `json:"old_string" description:"the exact text to replace; empty creates a new file with new_string as its content"`
	NewString   string `json:"new_string" description:"the replacement text"`
	ReplaceAll  bool   `json:"replace_all,omitempty" description:"replace every occurrence of old_string instead of requiring uniqueness"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

func agentEditTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"edit",
		agentEditToolDescription,
		func(ctx context.Context, p agentEditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if env.planningMode {
				return fantasy.NewTextResponse("Planning mode is ON. File modifications are currently blocked."), nil
			}
			if strings.TrimSpace(p.FilePath) == "" {
				return fantasy.NewTextErrorResponse("file_path is required"), nil
			}
			if p.OldString == p.NewString {
				return fantasy.NewTextErrorResponse("old_string and new_string are identical — nothing to do"), nil
			}
			path := env.absPath(p.FilePath)
			// When gateTodosBeforeMutate is on, no mutation before a task
			// list exists: editing is code-change intent, and the todos call
			// is the mandatory chokepoint where workflows are checked.
			// Refuse until todos has applied.
			//
			// Workflow plan files live under ask/plans/ and must be writable
			// before a workflow_run can be submitted. Allowing them through
			// here breaks the circular dependency between "edit the plan" and
			// "create a task list / run a workflow".
			if !isPathUnderWorkflowPlans(env.cwd, path) {
				if notice := env.requireTodosNotice(); notice != "" {
					return fantasy.NewTextResponse(notice), nil
				}
			}

			if p.OldString == "" {
				if _, err := os.Stat(path); err == nil {
					return fantasy.NewTextErrorResponse(path + " already exists; read it and edit with a non-empty old_string, or use write to overwrite"), nil
				}
				if denied := env.requestApproval(ctx, "edit", map[string]any{
					"file_path":   path,
					"old_string":  "",
					"new_string":  p.NewString,
					"description": p.Description,
				}); denied != nil {
					return *denied, nil
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return fantasy.NewTextErrorResponse("mkdir " + filepath.Dir(path) + ": " + err.Error()), nil
				}
				if err := os.WriteFile(path, []byte(p.NewString), 0o644); err != nil {
					return fantasy.NewTextErrorResponse("write " + path + ": " + err.Error()), nil
				}
				env.files.recordRead(path)
				env.emitFileDiff(path, "", p.NewString)
				return fantasy.NewTextResponse("created " + path), nil
			}

			info, err := os.Stat(path)
			if err != nil {
				if os.IsNotExist(err) {
					return fantasy.NewTextErrorResponse("file not found: " + path), nil
				}
				return fantasy.NewTextErrorResponse("stat " + path + ": " + err.Error()), nil
			}
			if info.IsDir() {
				return fantasy.NewTextErrorResponse(path + " is a directory"), nil
			}
			if guard := env.checkReadBeforeMutate(path, info.ModTime()); guard != "" {
				return fantasy.NewTextErrorResponse(guard), nil
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return fantasy.NewTextErrorResponse("read " + path + ": " + err.Error()), nil
			}
			content := string(data)

			// Normalize CRLF for matching, restore on write, so edits
			// written against the read tool's output (which strips \r via
			// line scanning) still land on Windows-flavored files.
			hadCRLF := strings.Contains(content, "\r\n")
			work := content
			oldStr, newStr := p.OldString, p.NewString
			if hadCRLF {
				work = strings.ReplaceAll(work, "\r\n", "\n")
				oldStr = strings.ReplaceAll(oldStr, "\r\n", "\n")
				newStr = strings.ReplaceAll(newStr, "\r\n", "\n")
			}

			count := strings.Count(work, oldStr)
			switch {
			case count == 0:
				return fantasy.NewTextErrorResponse("old_string not found in " + path + ". Make sure it matches the file content exactly, including whitespace and indentation. Re-read the file if it may have changed."), nil
			case count > 1 && !p.ReplaceAll:
				return fantasy.NewTextErrorResponse(fmt.Sprintf("old_string appears %d times in %s. Provide more surrounding context to make it unique, or set replace_all to true.", count, path)), nil
			}

			if denied := env.requestApproval(ctx, "edit", map[string]any{
				"file_path":   path,
				"old_string":  p.OldString,
				"new_string":  p.NewString,
				"replace_all": p.ReplaceAll,
				"description": p.Description,
			}); denied != nil {
				return *denied, nil
			}

			replaced := count
			if !p.ReplaceAll {
				replaced = 1
				work = strings.Replace(work, oldStr, newStr, 1)
			} else {
				work = strings.ReplaceAll(work, oldStr, newStr)
			}
			if hadCRLF {
				work = strings.ReplaceAll(work, "\n", "\r\n")
			}
			if err := os.WriteFile(path, []byte(work), info.Mode().Perm()); err != nil {
				return fantasy.NewTextErrorResponse("write " + path + ": " + err.Error()), nil
			}
			env.files.recordRead(path)
			env.emitFileDiff(path, content, work)
			if replaced == 1 {
				return fantasy.NewTextResponse("edited " + path), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("edited %s: %d replacements", path, replaced)), nil
		},
	)
}

// checkReadBeforeMutate enforces the read-before-edit contract on an
// existing file: the agent must have read the file this session, and
// the file must not have changed on disk since that read. Returns ""
// when the mutation may proceed, else the error text for the model.
func (env *agentToolEnv) checkReadBeforeMutate(path string, modTime time.Time) string {
	last := env.files.lastRead(path)
	if last.IsZero() {
		return "you must read " + path + " with the read tool before modifying it"
	}
	if modTime.After(last) {
		return path + " has changed on disk since you last read it — read it again before modifying"
	}
	return ""
}

// emitFileDiff computes a unified diff and pushes a toolDiffMsg so the
// UI renders agent edits exactly like claude/codex edits. No-op when
// the bodies are identical or the env has no emitter (tests).
func (env *agentToolEnv) emitFileDiff(path, oldBody, newBody string) {
	if env.emit == nil {
		return
	}
	diff := unifiedDiff(oldBody, newBody)
	if diff == "" {
		return
	}
	hunks := parseUnifiedDiff(diff)
	if len(hunks) == 0 {
		return
	}
	env.emit(toolDiffMsg{filePath: path, hunks: hunks})
}
