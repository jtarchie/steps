// Package depguardcover holds .golangci.yml's depguard rules to covering every internal/ package: Main excludes **/internal/**, so a package no rule names may import anything, and the acyclic graph CLAUDE.md promises has a hole nobody sees. Its test is the whole of it.
package depguardcover
