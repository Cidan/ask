package main

import (
	"testing"

	"github.com/Cidan/ask/internal/testhome"
)

func TestMain(m *testing.M) { testhome.Main(m, testhome.NoClaude()...) }
