package main

import (
	"github.com/Cidan/ask/pkg/tools"
)

func extractBaseCommand(command string) string {
	return tools.ExtractBaseCommand(command)
}

func applyBashFilter(command, raw string) (string, int) {
	return tools.ApplyBashFilter(command, raw)
}
