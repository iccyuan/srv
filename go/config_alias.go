package main

import "srv/internal/config"

// Config types + helpers moved to srv/internal/config. Type aliases
// here keep all the in-tree call sites that still live in package
// main (cmd handlers, client.go, daemon family, mcp_tools, ui, ...)
// compiling against the familiar names while subpackages that have
// already been extracted (jobs, install, completion, project) reach
// for the qualified `config.Config` / `config.Profile` directly.
//
// As features are themselves moved into internal/, their call sites
// flip to the qualified form one package at a time; once nothing in
// main still references these aliases we can drop this file.

type (
	Config    = config.Config
	Profile   = config.Profile
	TunnelDef = config.TunnelDef
)

var (
	LoadConfig        = config.Load
	SaveConfig        = config.Save
	ResolveProfile    = config.Resolve
	GetCwd            = config.GetCwd
	SetCwd            = config.SetCwd
	SetSessionProfile = config.SetSessionProfile
	newConfig         = config.New
)
