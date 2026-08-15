// Package ui is the bubbletea application shell: the root model, the message
// vocabulary and screen routing.
//
// Screen models live in subpackages and never touch a transport directly.
// They return tea.Cmd values that the root executes.
package ui
