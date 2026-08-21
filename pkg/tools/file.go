package tools

import (
	"bufio"
	"errors"
	"fmt"
	"google.golang.org/adk/v2/agent"
	"os"
	"path/filepath"
	"strings"
)

const ReadToolDescription = `Read a file from the filesystem. Returns the content with 1-based line numbers (cat -n format). Use offset/limit for large files; lines longer than 2000 chars are truncated. Reading a file is required before editing or overwriting it.`

type ReadParams struct {
	FilePath    string `json:"file_path" jsonschema:"absolute or cwd-relative path of the file to read"`
	Offset      int    `json:"offset,omitempty" jsonschema:"1-based line number to start reading from (default 1)"`
	Limit       int    `json:"limit,omitempty" jsonschema:"maximum number of lines to return (default 2000)"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// ReadResult is the read tool's response.
type ReadResult struct {
	Content    string `json:"content,omitempty" jsonschema:"file content, each line prefixed with its 1-based line number"`
	Lines      int    `json:"lines,omitempty" jsonschema:"number of lines returned"`
	NextOffset int    `json:"next_offset,omitempty" jsonschema:"offset to pass on the next call when the file was cut short"`
	Truncated  bool   `json:"truncated,omitempty" jsonschema:"true when the file has more content than was returned"`
}

// ReadTool returns the native read tool.
func ReadTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"read",
		ReadToolDescription,
		func(ctx agent.Context, p ReadParams) (ReadResult, error) {
			path := env.AbsPath(p.FilePath)
			info, err := os.Stat(path)
			if err != nil {
				if os.IsNotExist(err) {
					return ReadResult{}, fmt.Errorf("file not found: %s", path)
				}
				return ReadResult{}, fmt.Errorf("stat %s: %v", path, err)
			}
			if info.IsDir() {
				return ReadResult{}, fmt.Errorf("%s is a directory; use the ls tool instead", path)
			}
			if ImageExts[strings.ToLower(filepath.Ext(path))] {
				return ReadResult{}, errors.New("image files are not supported for raw text reading")
			}

			f, err := os.Open(path)
			if err != nil {
				return ReadResult{}, fmt.Errorf("open %s: %v", path, err)
			}
			defer f.Close()

			head := make([]byte, 8192)
			n, _ := f.Read(head)
			if LooksBinary(head[:n]) {
				return ReadResult{}, fmt.Errorf("%s looks like a binary file; reading it would not be useful", path)
			}
			if _, err := f.Seek(0, 0); err != nil {
				return ReadResult{}, fmt.Errorf("seek %s: %v", path, err)
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
				return ReadResult{}, fmt.Errorf("read %s: %v", path, err)
			}

			if env.Files != nil {
				env.Files.RecordRead(path)
			}
			if emitted == 0 {
				if offset > 1 {
					return ReadResult{Content: fmt.Sprintf("(no lines at offset %d; file has %d lines)", offset, lineNo)}, nil
				}
				return ReadResult{Content: "(empty file)"}, nil
			}

			res := ReadResult{Content: out.String(), Lines: emitted}
			if truncatedBytes || moreLines {
				res.Truncated = true
				res.NextOffset = offset + emitted
			}
			return res, nil
		},
	)
}

const WriteToolDescription = `Create or overwrite a file with the given content. Overwriting an existing file requires reading it first in this session. Parent directories are created automatically.`

type WriteParams struct {
	FilePath    string `json:"file_path" jsonschema:"absolute or cwd-relative path of the file to write"`
	Content     string `json:"content" jsonschema:"the full new content of the file"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// WriteResult is the write tool's response.
type WriteResult struct {
	Path     string `json:"path,omitempty" jsonschema:"absolute path written"`
	Created  bool   `json:"created,omitempty" jsonschema:"true when the file did not exist before"`
	Bytes    int    `json:"bytes,omitempty" jsonschema:"size of the file after the write"`
	NoChange bool   `json:"no_change,omitempty" jsonschema:"true when the file already had exactly this content"`
	Notice   string `json:"notice,omitempty" jsonschema:"guidance the caller must act on before retrying"`
}

// WriteTool returns the native write tool.
func WriteTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"write",
		WriteToolDescription,
		func(ctx agent.Context, p WriteParams) (WriteResult, error) {
			if strings.TrimSpace(p.FilePath) == "" {
				return WriteResult{}, errors.New("file_path is required")
			}
			path := env.AbsPath(p.FilePath)
			if notice := env.RequireTodosNotice(); notice != "" {
				return WriteResult{Notice: notice}, nil
			}
			oldContent := ""
			mode := os.FileMode(0o644)
			if info, err := os.Stat(path); err == nil {
				if info.IsDir() {
					return WriteResult{}, errors.New(path + " is a directory")
				}
				if guard := env.CheckReadBeforeMutate(path, info.ModTime()); guard != "" {
					return WriteResult{}, errors.New(guard)
				}
				mode = info.Mode().Perm()
				data, err := os.ReadFile(path)
				if err != nil {
					return WriteResult{}, errors.New("read " + path + ": " + err.Error())
				}
				oldContent = string(data)
				if oldContent == p.Content {
					return WriteResult{Path: path, NoChange: true}, nil
				}
			}

			if denied := env.ApprovalDenied(ctx, "write", map[string]any{
				"file_path":   path,
				"content":     p.Content,
				"description": p.Description,
			}); denied != "" {
				return WriteResult{}, errors.New(denied)
			}

			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return WriteResult{}, errors.New("mkdir " + filepath.Dir(path) + ": " + err.Error())
			}
			if err := os.WriteFile(path, []byte(p.Content), mode); err != nil {
				return WriteResult{}, errors.New("write " + path + ": " + err.Error())
			}
			if env.Files != nil {
				env.Files.RecordRead(path)
			}
			env.EmitFileDiff(path, oldContent, p.Content)
			return WriteResult{Path: path, Created: oldContent == "", Bytes: len(p.Content)}, nil
		},
	)
}

const EditToolDescription = `Replace an exact string in a file. old_string must match the file content exactly, including whitespace and indentation, and must be unique in the file unless replace_all is set. Use an empty old_string to create a new file. The file must have been read in this session before editing.`

type EditParams struct {
	FilePath    string `json:"file_path" jsonschema:"absolute or cwd-relative path of the file to edit"`
	OldString   string `json:"old_string" jsonschema:"the exact text to replace; empty creates a new file with new_string as its content"`
	NewString   string `json:"new_string" jsonschema:"the replacement text"`
	ReplaceAll  bool   `json:"replace_all,omitempty" jsonschema:"replace every occurrence of old_string instead of requiring uniqueness"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// EditResult is the edit tool's response.
type EditResult struct {
	Path         string `json:"path,omitempty" jsonschema:"absolute path edited"`
	Created      bool   `json:"created,omitempty" jsonschema:"true when the edit created the file"`
	Replacements int    `json:"replacements,omitempty" jsonschema:"number of occurrences replaced"`
	Notice       string `json:"notice,omitempty" jsonschema:"guidance the caller must act on before retrying"`
}

// EditTool returns the native edit tool.
func EditTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"edit",
		EditToolDescription,
		func(ctx agent.Context, p EditParams) (EditResult, error) {
			if strings.TrimSpace(p.FilePath) == "" {
				return EditResult{}, errors.New("file_path is required")
			}
			if p.OldString == p.NewString {
				return EditResult{}, errors.New("old_string and new_string are identical — nothing to do")
			}
			path := env.AbsPath(p.FilePath)
			if notice := env.RequireTodosNotice(); notice != "" {
				return EditResult{Notice: notice}, nil
			}

			if p.OldString == "" {
				if _, err := os.Stat(path); err == nil {
					return EditResult{}, errors.New(path + " already exists; read it and edit with a non-empty old_string, or use write to overwrite")
				}
				if denied := env.ApprovalDenied(ctx, "edit", map[string]any{
					"file_path":   path,
					"old_string":  "",
					"new_string":  p.NewString,
					"description": p.Description,
				}); denied != "" {
					return EditResult{}, errors.New(denied)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return EditResult{}, errors.New("mkdir " + filepath.Dir(path) + ": " + err.Error())
				}
				if err := os.WriteFile(path, []byte(p.NewString), 0o644); err != nil {
					return EditResult{}, errors.New("write " + path + ": " + err.Error())
				}
				if env.Files != nil {
					env.Files.RecordRead(path)
				}
				env.EmitFileDiff(path, "", p.NewString)
				return EditResult{Path: path, Created: true, Replacements: 1}, nil
			}

			info, err := os.Stat(path)
			if err != nil {
				if os.IsNotExist(err) {
					return EditResult{}, errors.New("file not found: " + path)
				}
				return EditResult{}, errors.New("stat " + path + ": " + err.Error())
			}
			if info.IsDir() {
				return EditResult{}, errors.New(path + " is a directory")
			}
			if guard := env.CheckReadBeforeMutate(path, info.ModTime()); guard != "" {
				return EditResult{}, errors.New(guard)
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return EditResult{}, errors.New("read " + path + ": " + err.Error())
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
				return EditResult{}, errors.New("old_string not found in " + path + ". Make sure it matches the file content exactly, including whitespace and indentation. Re-read the file if it may have changed.")
			case count > 1 && !p.ReplaceAll:
				return EditResult{}, fmt.Errorf("old_string appears %d times in %s. Provide more surrounding context to make it unique, or set replace_all to true.", count, path)
			}

			if denied := env.ApprovalDenied(ctx, "edit", map[string]any{
				"file_path":   path,
				"old_string":  p.OldString,
				"new_string":  p.NewString,
				"replace_all": p.ReplaceAll,
				"description": p.Description,
			}); denied != "" {
				return EditResult{}, errors.New(denied)
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
				return EditResult{}, errors.New("write " + path + ": " + err.Error())
			}
			if env.Files != nil {
				env.Files.RecordRead(path)
			}
			env.EmitFileDiff(path, content, work)
			return EditResult{Path: path, Replacements: replaced}, nil
		},
	)
}
