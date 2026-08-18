package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cidan/ask/pkg/workflow"
)

const ReadToolDescription = `Read a file from the filesystem. Returns the content with 1-based line numbers (cat -n format). Use offset/limit for large files; lines longer than 2000 chars are truncated. Reading a file is required before editing or overwriting it.`

type ReadParams struct {
	FilePath    string `json:"file_path" description:"absolute or cwd-relative path of the file to read"`
	Offset      int    `json:"offset,omitempty" description:"1-based line number to start reading from (default 1)"`
	Limit       int    `json:"limit,omitempty" description:"maximum number of lines to return (default 2000)"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// ReadTool returns the native read tool.
func ReadTool(env *ToolEnv) Tool {
	return NewTool(
		"read",
		ReadToolDescription,
		func(ctx context.Context, p ReadParams) (ToolResponse, error) {
			path := env.AbsPath(p.FilePath)
			info, err := os.Stat(path)
			if err != nil {
				if os.IsNotExist(err) {
					return NewTextErrorResponse("file not found: " + path), nil
				}
				return NewTextErrorResponse("stat " + path + ": " + err.Error()), nil
			}
			if info.IsDir() {
				return NewTextErrorResponse(path + " is a directory; use the ls tool instead"), nil
			}
			if ImageExts[strings.ToLower(filepath.Ext(path))] {
				return NewTextErrorResponse("image files are not supported for raw text reading"), nil
			}

			f, err := os.Open(path)
			if err != nil {
				return NewTextErrorResponse("open " + path + ": " + err.Error()), nil
			}
			defer f.Close()

			head := make([]byte, 8192)
			n, _ := f.Read(head)
			if LooksBinary(head[:n]) {
				return NewTextErrorResponse(path + " looks like a binary file; reading it would not be useful"), nil
			}
			if _, err := f.Seek(0, 0); err != nil {
				return NewTextErrorResponse("seek " + path + ": " + err.Error()), nil
			}

			offset := max(p.Offset, 1)
			limit := p.Limit
			if limit <= 0 {
				limit = MaxReadLines
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
				fmt.Fprintf(&out, "%6d\t%s\n", lineNo, TruncateLine(sc.Text()))
				emitted++
				if out.Len() >= MaxReadBytes {
					truncatedBytes = true
					break
				}
			}
			if err := sc.Err(); err != nil {
				return NewTextErrorResponse("read " + path + ": " + err.Error()), nil
			}

			if env.Files != nil {
				env.Files.RecordRead(path)
			}
			if emitted == 0 {
				if offset > 1 {
					return NewTextResponse(fmt.Sprintf("(no lines at offset %d; file has %d lines)", offset, lineNo)), nil
				}
				return NewTextResponse("(empty file)"), nil
			}
			body := out.String()
			switch {
			case truncatedBytes:
				body += fmt.Sprintf("(output capped at %d bytes; continue with offset %d)\n", MaxReadBytes, offset+emitted)
			case moreLines:
				body += fmt.Sprintf("(file has more lines; continue with offset %d)\n", offset+emitted)
			}
			return NewTextResponse(body), nil
		},
	)
}

const WriteToolDescription = `Create or overwrite a file with the given content. Overwriting an existing file requires reading it first in this session. Parent directories are created automatically.`

type WriteParams struct {
	FilePath    string `json:"file_path" description:"absolute or cwd-relative path of the file to write"`
	Content     string `json:"content" description:"the full new content of the file"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// WriteTool returns the native write tool.
func WriteTool(env *ToolEnv) Tool {
	return NewTool(
		"write",
		WriteToolDescription,
		func(ctx context.Context, p WriteParams) (ToolResponse, error) {
			if strings.TrimSpace(p.FilePath) == "" {
				return NewTextErrorResponse("file_path is required"), nil
			}
			path := env.AbsPath(p.FilePath)
			if !workflow.IsPathUnderWorkflowPlans(env.Cwd, path) {
				if notice := env.RequireTodosNotice(); notice != "" {
					return NewTextResponse(notice), nil
				}
			}
			oldContent := ""
			mode := os.FileMode(0o644)
			if info, err := os.Stat(path); err == nil {
				if info.IsDir() {
					return NewTextErrorResponse(path + " is a directory"), nil
				}
				if guard := env.CheckReadBeforeMutate(path, info.ModTime()); guard != "" {
					return NewTextErrorResponse(guard), nil
				}
				mode = info.Mode().Perm()
				data, err := os.ReadFile(path)
				if err != nil {
					return NewTextErrorResponse("read " + path + ": " + err.Error()), nil
				}
				oldContent = string(data)
				if oldContent == p.Content {
					return NewTextResponse("no change: " + path + " already has that exact content"), nil
				}
			}

			if denied := env.RequestApproval(ctx, "write", map[string]any{
				"file_path":   path,
				"content":     p.Content,
				"description": p.Description,
			}); denied != nil {
				return *denied, nil
			}

			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return NewTextErrorResponse("mkdir " + filepath.Dir(path) + ": " + err.Error()), nil
			}
			if err := os.WriteFile(path, []byte(p.Content), mode); err != nil {
				return NewTextErrorResponse("write " + path + ": " + err.Error()), nil
			}
			if env.Files != nil {
				env.Files.RecordRead(path)
			}
			env.EmitFileDiff(path, oldContent, p.Content)
			if oldContent == "" {
				return NewTextResponse("created " + path), nil
			}
			return NewTextResponse("updated " + path), nil
		},
	)
}

const EditToolDescription = `Replace an exact string in a file. old_string must match the file content exactly, including whitespace and indentation, and must be unique in the file unless replace_all is set. Use an empty old_string to create a new file. The file must have been read in this session before editing.`

type EditParams struct {
	FilePath    string `json:"file_path" description:"absolute or cwd-relative path of the file to edit"`
	OldString   string `json:"old_string" description:"the exact text to replace; empty creates a new file with new_string as its content"`
	NewString   string `json:"new_string" description:"the replacement text"`
	ReplaceAll  bool   `json:"replace_all,omitempty" description:"replace every occurrence of old_string instead of requiring uniqueness"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// EditTool returns the native edit tool.
func EditTool(env *ToolEnv) Tool {
	return NewTool(
		"edit",
		EditToolDescription,
		func(ctx context.Context, p EditParams) (ToolResponse, error) {
			if strings.TrimSpace(p.FilePath) == "" {
				return NewTextErrorResponse("file_path is required"), nil
			}
			if p.OldString == p.NewString {
				return NewTextErrorResponse("old_string and new_string are identical — nothing to do"), nil
			}
			path := env.AbsPath(p.FilePath)
			if !workflow.IsPathUnderWorkflowPlans(env.Cwd, path) {
				if notice := env.RequireTodosNotice(); notice != "" {
					return NewTextResponse(notice), nil
				}
			}

			if p.OldString == "" {
				if _, err := os.Stat(path); err == nil {
					return NewTextErrorResponse(path + " already exists; read it and edit with a non-empty old_string, or use write to overwrite"), nil
				}
				if denied := env.RequestApproval(ctx, "edit", map[string]any{
					"file_path":   path,
					"old_string":  "",
					"new_string":  p.NewString,
					"description": p.Description,
				}); denied != nil {
					return *denied, nil
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return NewTextErrorResponse("mkdir " + filepath.Dir(path) + ": " + err.Error()), nil
				}
				if err := os.WriteFile(path, []byte(p.NewString), 0o644); err != nil {
					return NewTextErrorResponse("write " + path + ": " + err.Error()), nil
				}
				if env.Files != nil {
					env.Files.RecordRead(path)
				}
				env.EmitFileDiff(path, "", p.NewString)
				return NewTextResponse("created " + path), nil
			}

			info, err := os.Stat(path)
			if err != nil {
				if os.IsNotExist(err) {
					return NewTextErrorResponse("file not found: " + path), nil
				}
				return NewTextErrorResponse("stat " + path + ": " + err.Error()), nil
			}
			if info.IsDir() {
				return NewTextErrorResponse(path + " is a directory"), nil
			}
			if guard := env.CheckReadBeforeMutate(path, info.ModTime()); guard != "" {
				return NewTextErrorResponse(guard), nil
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return NewTextErrorResponse("read " + path + ": " + err.Error()), nil
			}
			content := string(data)

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
				return NewTextErrorResponse("old_string not found in " + path + ". Make sure it matches the file content exactly, including whitespace and indentation. Re-read the file if it may have changed."), nil
			case count > 1 && !p.ReplaceAll:
				return NewTextErrorResponse(fmt.Sprintf("old_string appears %d times in %s. Provide more surrounding context to make it unique, or set replace_all to true.", count, path)), nil
			}

			if denied := env.RequestApproval(ctx, "edit", map[string]any{
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
				return NewTextErrorResponse("write " + path + ": " + err.Error()), nil
			}
			if env.Files != nil {
				env.Files.RecordRead(path)
			}
			env.EmitFileDiff(path, content, work)
			if replaced == 1 {
				return NewTextResponse("edited " + path), nil
			}
			return NewTextResponse(fmt.Sprintf("edited %s: %d replacements", path, replaced)), nil
		},
	)
}
