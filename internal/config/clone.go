package config

// Clone returns a detached configuration snapshot. Config contains several
// slices, maps and pointer-valued switches; a struct assignment alone would
// let a settings update mutate a runtime that is already being assembled.
func (c Config) Clone() Config {
	out := c
	out.Tools.Enabled = append([]string(nil), c.Tools.Enabled...)
	out.Tools.RunCommand = c.Tools.RunCommand
	out.Terminal.Args = append([]string(nil), c.Terminal.Args...)
	out.Subagent.ExternalProviders = make(map[string]ExternalProviderConfig, len(c.Subagent.ExternalProviders))
	for key, value := range c.Subagent.ExternalProviders {
		out.Subagent.ExternalProviders[key] = value
	}
	out.Skill.Dirs = append([]string(nil), c.Skill.Dirs...)
	out.Interact.SensitiveTools = append([]string(nil), c.Interact.SensitiveTools...)
	out.Mcp.Servers = make([]McpServer, len(c.Mcp.Servers))
	for i, server := range c.Mcp.Servers {
		out.Mcp.Servers[i] = server
		out.Mcp.Servers[i].Args = append([]string(nil), server.Args...)
		out.Mcp.Servers[i].Headers = make(map[string]string, len(server.Headers))
		for key, value := range server.Headers {
			out.Mcp.Servers[i].Headers[key] = value
		}
	}
	out.LSP.Args = append([]string(nil), c.LSP.Args...)
	out.LSP.Extensions = make(map[string]string, len(c.LSP.Extensions))
	for key, value := range c.LSP.Extensions {
		out.LSP.Extensions[key] = value
	}
	out.Hooks.Args = append([]string(nil), c.Hooks.Args...)
	out.Hooks.Events = append([]string(nil), c.Hooks.Events...)
	out.Ralph = c.Ralph
	out.Eval = c.Eval
	out.Compaction = c.Compaction
	out.Plan = c.Plan
	out.Spill = c.Spill
	out.Code = c.Code
	out.Web = c.Web
	out.LLM = c.LLM
	out.LLM.ThinkingBudgets = make(map[string]int, len(c.LLM.ThinkingBudgets))
	for key, value := range c.LLM.ThinkingBudgets {
		out.LLM.ThinkingBudgets[key] = value
	}
	out.Workspace = c.Workspace
	out.WebServer = c.WebServer
	out.FsSearch = c.FsSearch
	out.SessionQuery = c.SessionQuery
	return cloneConfigPointers(out)
}

func cloneConfigPointers(c Config) Config {
	c.Eval.Enabled = cloneBool(c.Eval.Enabled)
	c.Eval.ManualFallback = cloneBool(c.Eval.ManualFallback)
	c.Ralph.Enabled = cloneBool(c.Ralph.Enabled)
	c.Subagent.Enabled = cloneBool(c.Subagent.Enabled)
	c.Subagent.ACPEnabled = cloneBool(c.Subagent.ACPEnabled)
	c.Compaction.Enabled = cloneBool(c.Compaction.Enabled)
	c.Skill.Enabled = cloneBool(c.Skill.Enabled)
	c.Schedule.Enabled = cloneBool(c.Schedule.Enabled)
	c.Plan.Enabled = cloneBool(c.Plan.Enabled)
	c.Plan.AllowParallelInProgress = cloneBool(c.Plan.AllowParallelInProgress)
	c.Plan.BlockedAfterConsecutiveRounds = cloneInt(c.Plan.BlockedAfterConsecutiveRounds)
	c.Spill.Enabled = cloneBool(c.Spill.Enabled)
	c.Spill.AutoSpill = cloneBool(c.Spill.AutoSpill)
	c.Interact.Enabled = cloneBool(c.Interact.Enabled)
	c.Code.Enabled = cloneBool(c.Code.Enabled)
	c.Mcp.Enabled = cloneBool(c.Mcp.Enabled)
	c.Mcp.ACPEnabled = cloneBool(c.Mcp.ACPEnabled)
	c.Fs.Enabled = cloneBool(c.Fs.Enabled)
	c.Web.Enabled = cloneBool(c.Web.Enabled)
	c.Terminal.Enabled = cloneBool(c.Terminal.Enabled)
	c.Terminal.ACPEnabled = cloneBool(c.Terminal.ACPEnabled)
	return c
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}
