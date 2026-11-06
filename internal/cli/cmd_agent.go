package cli

import (
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
)

// agentCommands: ui, mcp, skills (cli.html#group-agent).
func (a *App) agentCommands() []*cobra.Command {
	ui := command("agent", "ui", "Open the web UI in your browser", "", "read")
	ui.Args = cobra.NoArgs
	ui.Flags().Bool("no-browser", false, "print the URL; open no browser")
	ui.Flags().String("listen", "", "a loopback address only (default 127.0.0.1:0, also REPOCTL_UI_LISTEN)")
	ui.Flags().Bool("stop", false, "stop the server")
	ui.Flags().String("remove", "", "remove a set from the set list")
	ui.RunE = a.run(hUI)

	mcp := command("agent", "mcp", "Run the MCP server for LLM agents", "", "read")
	mcp.Annotations[annoNoTarget] = "true"
	start := sub("start", "Run the MCP server on stdio", "", "read")
	start.Args = cobra.NoArgs
	start.Flags().String("tools", "all", "the tools: read, all or NAME,...")
	start.RunE = a.run(hMCPStart)
	tools := sub("tools", "Print the tool definitions", "", "read")
	tools.Args = cobra.NoArgs
	tools.Flags().String("tools", "all", "the tools: read, all or NAME,...")
	tools.RunE = a.run(hMCPTools)
	mcp.AddCommand(start, tools)

	skills := command("agent", "skills", "List, install and remove the agent skills", "", "read")
	skills.Annotations[annoNoTarget] = "true"
	sls := sub("ls", "List the skills and their installed copies", api.KindSkill, "read")
	sls.Args = cobra.NoArgs
	sls.RunE = a.run(hSkillsLs)
	sshow := sub("show NAME", "Print one skill", api.KindSkill, "read")
	sshow.Args = cobra.ExactArgs(1)
	sshow.RunE = a.run(hSkillsShow)
	spath := sub("path", "Print the directory of the skills", "", "read")
	spath.Args = cobra.NoArgs
	spath.RunE = a.run(hSkillsPath)
	sguard := sub("guard", "The PreToolUse hook of Claude Code", "", "read")
	sguard.Args = cobra.NoArgs
	sguard.RunE = a.run(hSkillsGuard)
	sinst := sub("install [NAME...]", "Install the skills, the deny list and the guard hook", api.KindSkill, "local write")
	sinst.Flags().Bool("user", false, "install into ~/.claude/skills (default)")
	sinst.Flags().Bool("project", false, "install into SET/.claude/skills")
	sinst.RunE = a.run(hSkillsInstall)
	sun := sub("uninstall [NAME...]", "Remove the skills", api.KindSkill, "local write")
	sun.Flags().Bool("user", false, "remove from ~/.claude/skills (default)")
	sun.Flags().Bool("project", false, "remove from SET/.claude/skills")
	sun.RunE = a.run(hSkillsUninstall)
	skills.AddCommand(sls, sshow, spath, sguard, sinst, sun)

	return []*cobra.Command{ui, mcp, skills}
}
