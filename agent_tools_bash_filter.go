package main

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	ansiEscapeRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
	npmWarnRegex    = regexp.MustCompile(`(?m)^npm WARN.*$`)
	goDownloadRegex = regexp.MustCompile(`(?m)^go: downloading.*$`)
	gitNoiseRegex   = regexp.MustCompile(`(?m)^(?:remote: .*|To .*|From .*|\s*\[(?:new branch|new tag|up to date)\].*)$`)
)

// extractBaseCommand parses the executable and first arguments that identify the tool action
func extractBaseCommand(command string) string {
	parts := regexp.MustCompile(`(&&|\|\||;|\|)`).Split(command, -1)
	if len(parts) == 0 {
		return ""
	}
	lastPart := strings.TrimSpace(parts[len(parts)-1])
	fields := strings.Fields(lastPart)
	
	var cmdFields []string
	for _, f := range fields {
		if strings.Contains(f, "=") && !strings.Contains(f, "/") {
			continue
		}
		cmdFields = append(cmdFields, f)
	}
	
	if len(cmdFields) == 0 {
		return ""
	}

	base := cmdFields[0]
	if len(cmdFields) > 1 {
		switch base {
		case "go", "npm", "git", "yarn", "cargo", "pnpm":
			return base + " " + cmdFields[1]
		}
	}
	return base
}

// applyBashFilter compresses command output to save tokens by removing ANSI escape codes
// and known noisy output lines for standard tools.
func applyBashFilter(command string, rawOutput string) (string, int) {
	if rawOutput == "" {
		return "", 0
	}

	filtered := ansiEscapeRegex.ReplaceAllString(rawOutput, "")

	baseCmd := extractBaseCommand(command)
	switch baseCmd {
	case "npm install", "npm i", "npm ci":
		filtered = npmWarnRegex.ReplaceAllString(filtered, "")
	case "go build", "go test", "go run", "go get", "go mod":
		filtered = goDownloadRegex.ReplaceAllString(filtered, "")
	case "git push", "git fetch", "git pull", "git status":
		filtered = gitNoiseRegex.ReplaceAllString(filtered, "")
	}

	var result []string
	lines := strings.Split(filtered, "\n")
	wasEmpty := false
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t\r")
		isEmpty := trimmed == ""
		if isEmpty && wasEmpty {
			continue
		}
		result = append(result, trimmed)
		wasEmpty = isEmpty
	}

	for len(result) > 0 && result[0] == "" {
		result = result[1:]
	}
	for len(result) > 0 && result[len(result)-1] == "" {
		result = result[:len(result)-1]
	}

	const maxLines = 1000
	if len(result) > maxLines {
		half := maxLines / 2
		newResult := make([]string, 0, maxLines+1)
		newResult = append(newResult, result[:half]...)
		newResult = append(newResult, fmt.Sprintf("... %d lines truncated to save tokens ...", len(result)-maxLines))
		newResult = append(newResult, result[len(result)-half:]...)
		result = newResult
	}

	filteredOutput := strings.Join(result, "\n")
	if len(filteredOutput) > 0 && strings.HasSuffix(rawOutput, "\n") {
		filteredOutput += "\n"
	}
	if len(filteredOutput) == 0 && len(rawOutput) > 0 {
		// fallback to preserve minimal context if everything was filtered out
		filteredOutput = "(Command output was entirely filtered out)\n"
	}

	tokensSaved := (len(rawOutput) - len(filteredOutput)) / 4
	if tokensSaved < 0 {
		tokensSaved = 0
	}

	return filteredOutput, tokensSaved
}
