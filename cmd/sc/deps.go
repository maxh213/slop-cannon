//go:build deps

// This file is never compiled (build tag "deps"). It exists so that
// "go mod tidy" keeps the D-01 stack and the shared test helpers in go.mod
// before any package imports them, avoiding go.mod churn between agents
// working in parallel. Delete an import here once a real package uses it.
package main

import (
	_ "github.com/charmbracelet/bubbles/spinner"
	_ "github.com/charmbracelet/bubbletea"
	_ "github.com/charmbracelet/harmonica"
	_ "github.com/charmbracelet/lipgloss"
	_ "github.com/charmbracelet/x/exp/teatest"
	_ "github.com/pelletier/go-toml/v2"
	_ "github.com/spf13/cobra"
	_ "github.com/stretchr/testify/require"
)
