package tools

import (
	"bufio"
	"errors"
	"fmt"
	"google.golang.org/adk/v2/agent"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const ReadToolDescription = `Read files from the filesystem. Returns the content with 1-based line numbers (cat -n format); lines longer than 2000 chars are truncated. Read one file with file_path (offset/limit for a slice of it), or many at once with files, where each file takes any number of line ranges, and run grep searches in the same call with grep. Put everything you already know you need into one call: each extra call resends the whole conversation. In a batched call every file range and every search comes back under its own "==>" heading, and a file that fails reports its error there without failing the rest. Reading a file is required before editing or overwriting it. Image files (png, jpg, gif, webp, bmp, tiff) are decoded and shown to you visually instead of as text, when the model has vision. Use this tool, never cat, head, tail, or sed -n through bash.`

// ReadRange is one slice of a file for the read tool.
type ReadRange struct {
	Offset int `json:"offset,omitempty" jsonschema:"1-based line number to start from (default 1)"`
	Limit  int `json:"limit,omitempty" jsonschema:"maximum number of lines to return (default 2000)"`
}

// ReadFile is one file of a batched read.
type ReadFile struct {
	FilePath string      `json:"file_path" jsonschema:"absolute or cwd-relative path of the file to read"`
	Ranges   []ReadRange `json:"ranges,omitempty" jsonschema:"line ranges to return (default: from line 1, up to 2000 lines)"`
}

type ReadParams struct {
	FilePath    string       `json:"file_path,omitempty" jsonschema:"absolute or cwd-relative path of one file to read"`
	Offset      int          `json:"offset,omitempty" jsonschema:"1-based line number to start reading file_path from (default 1)"`
	Limit       int          `json:"limit,omitempty" jsonschema:"maximum number of lines of file_path to return (default 2000)"`
	Files       []ReadFile   `json:"files,omitempty" jsonschema:"several files to read in this one call, each with any number of line ranges"`
	Grep        []GrepSearch `json:"grep,omitempty" jsonschema:"grep searches to run in this same call, each with the grep tool's fields"`
	Description string       `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// ReadTargets lists the files a read call asks for, file_path first.
func (p ReadParams) ReadTargets() []ReadFile {
	if p.FilePath == "" {
		return p.Files
	}
	first := ReadFile{FilePath: p.FilePath, Ranges: []ReadRange{{Offset: p.Offset, Limit: p.Limit}}}
	return append([]ReadFile{first}, p.Files...)
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
			files := p.ReadTargets()
			if len(files) == 0 && len(p.Grep) == 0 {
				return ReadResult{}, errors.New("file_path, files, or grep is required")
			}
			if len(files) == 1 && len(files[0].Ranges) <= 1 && len(p.Grep) == 0 {
				var r ReadRange
				if len(files[0].Ranges) == 1 {
					r = files[0].Ranges[0]
				}
				return readOne(env, files[0].FilePath, r)
			}
			return readBatch(ctx, env, files, p.Grep)
		},
	)
}

// openTextFile opens path for reading as text, refusing a missing file, a
// directory, and a binary file. image is true, with no file opened, for an
// image extension.
func openTextFile(path string) (f *os.File, image bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, fmt.Errorf("file not found: %s", path)
		}
		return nil, false, fmt.Errorf("stat %s: %v", path, err)
	}
	if info.IsDir() {
		return nil, false, fmt.Errorf("%s is a directory; use the ls tool instead", path)
	}
	if ImageExts[strings.ToLower(filepath.Ext(path))] {
		return nil, true, nil
	}
	f, err = os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open %s: %v", path, err)
	}
	head := make([]byte, 8192)
	n, _ := f.Read(head)
	if LooksBinary(head[:n]) {
		f.Close()
		return nil, false, fmt.Errorf("%s looks like a binary file; reading it would not be useful", path)
	}
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return nil, false, fmt.Errorf("seek %s: %v", path, err)
	}
	return f, false, nil
}

func newLineScanner(f *os.File) *bufio.Scanner {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return sc
}

func readOne(env *ToolEnv, filePath string, r ReadRange) (ReadResult, error) {
	path := env.AbsPath(filePath)
	f, image, err := openTextFile(path)
	if err != nil {
		return ReadResult{}, err
	}
	if image {
		return readImageFile(env, path)
	}
	defer f.Close()

	offset := max(r.Offset, 1)
	limit := r.Limit
	if limit <= 0 {
		limit = MaxReadLines
	}

	var out strings.Builder
	sc := newLineScanner(f)
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
}

// lineSpan is a 1-based, inclusive run of lines.
type lineSpan struct{ first, last int }

// mergeRanges turns ranges into sorted spans with overlapping and adjacent
// ones joined, so no line is returned twice.
func mergeRanges(ranges []ReadRange) []lineSpan {
	if len(ranges) == 0 {
		ranges = []ReadRange{{}}
	}
	spans := make([]lineSpan, 0, len(ranges))
	for _, r := range ranges {
		first := max(r.Offset, 1)
		limit := r.Limit
		if limit <= 0 {
			limit = MaxReadLines
		}
		last := first + limit - 1
		if last < first {
			last = math.MaxInt
		}
		spans = append(spans, lineSpan{first, last})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].first < spans[j].first })
	merged := spans[:1]
	for _, s := range spans[1:] {
		cur := &merged[len(merged)-1]
		if s.first <= cur.last+1 {
			cur.last = max(cur.last, s.last)
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// batchSection is one file range or search of a batched read.
type batchSection struct {
	heading string
	body    string
	lines   int
}

// readSpans reads the spans of one text file, each under its own heading,
// stopping once budget bytes are written.
func readSpans(path string, f *os.File, spans []lineSpan, budget int) ([]batchSection, bool, error) {
	sc := newLineScanner(f)
	sections := make([]batchSection, len(spans))
	bodies := make([]strings.Builder, len(spans))
	written := 0
	cut := -1
	lineNo := 0
	si := 0
	for sc.Scan() {
		lineNo++
		for si < len(spans) && lineNo > spans[si].last {
			si++
		}
		if si == len(spans) || cut >= 0 || lineNo < spans[si].first {
			continue
		}
		before := bodies[si].Len()
		fmt.Fprintf(&bodies[si], "%6d\t%s\n", lineNo, TruncateLine(sc.Text()))
		written += bodies[si].Len() - before
		sections[si].lines++
		if written >= budget {
			cut = lineNo
		}
	}
	if err := sc.Err(); err != nil {
		return nil, false, fmt.Errorf("read %s: %v", path, err)
	}
	total := lineNo
	truncated := cut >= 0
	for i, s := range spans {
		sec := &sections[i]
		switch {
		case total == 0:
			sec.heading, sec.body = path, "(empty file)\n"
			return sections[:1], false, nil
		case sec.lines == 0 && s.first > total:
			sec.heading = path
			sec.body = fmt.Sprintf("(no lines at offset %d; file has %d lines)\n", s.first, total)
		case sec.lines == 0:
			sec.heading = path
			sec.body = fmt.Sprintf("(not read: this call's %d-byte output cap was reached; read lines %d-%d in another call)\n", MaxReadBytes, s.first, s.last)
		default:
			last := s.first + sec.lines - 1
			sec.heading = fmt.Sprintf("%s lines %d-%d of %d", path, s.first, last, total)
			sec.body = bodies[i].String()
			if cut >= 0 && cut == last && last < min(s.last, total) {
				sec.body += fmt.Sprintf("(cut at the %d-byte output cap; continue at offset %d)\n", MaxReadBytes, last+1)
			}
		}
	}
	return sections, truncated, nil
}

func readBatch(ctx agent.Context, env *ToolEnv, files []ReadFile, searches []GrepSearch) (ReadResult, error) {
	var sections []batchSection
	var errs []string
	written := 0
	ok := 0
	truncated := false
	fail := func(heading string, err error) {
		errs = append(errs, err.Error())
		sections = append(sections, batchSection{heading: heading, body: "error: " + err.Error() + "\n"})
	}
	full := func(heading, what string) {
		truncated = true
		sections = append(sections, batchSection{heading: heading, body: fmt.Sprintf("(not read: this call's %d-byte output cap was reached; %s in another call)\n", MaxReadBytes, what)})
	}

	for _, rf := range files {
		if strings.TrimSpace(rf.FilePath) == "" {
			fail("(no file_path)", errors.New("file_path is required"))
			continue
		}
		path := env.AbsPath(rf.FilePath)
		if written >= MaxReadBytes {
			full(path, "read it")
			continue
		}
		f, image, err := openTextFile(path)
		if err != nil {
			fail(path, err)
			continue
		}
		if image {
			res, err := readImageFile(env, path)
			if err != nil {
				fail(path, err)
				continue
			}
			ok++
			sections = append(sections, batchSection{heading: path, body: res.Content + "\n"})
			continue
		}
		got, cut, err := readSpans(path, f, mergeRanges(rf.Ranges), MaxReadBytes-written)
		f.Close()
		if err != nil {
			fail(path, err)
			continue
		}
		if env.Files != nil {
			env.Files.RecordRead(path)
		}
		ok++
		truncated = truncated || cut
		for _, sec := range got {
			written += len(sec.body)
			sections = append(sections, sec)
		}
	}

	for _, s := range searches {
		patterns := s.AllPatterns()
		if len(patterns) == 0 {
			fail("grep", errors.New("pattern or patterns is required"))
			continue
		}
		root := env.AbsPath(s.Path)
		for _, pat := range patterns {
			heading := grepLabel(pat, s)
			if written >= MaxReadBytes {
				full(heading, "run this search")
				continue
			}
			body, errText := grepPattern(ctx, RgPath, pat, s, root)
			if errText != "" {
				fail(heading, errors.New(errText))
				continue
			}
			ok++
			written += len(body)
			sections = append(sections, batchSection{heading: heading, body: body})
		}
	}

	if ok == 0 {
		return ReadResult{}, errors.New(strings.Join(errs, "; "))
	}
	var out strings.Builder
	lines := 0
	for i, sec := range sections {
		if i > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(&out, "==> %s <==\n%s\n", sec.heading, strings.TrimRight(sec.body, "\n"))
		lines += sec.lines
	}
	return ReadResult{Content: out.String(), Lines: lines, Truncated: truncated}, nil
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
