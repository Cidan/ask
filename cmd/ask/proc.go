package main

import (
	"os"
)

// sessionArgs bundles the model's current state into the provider-shaped
// arg struct used by Provider.StartSession / ProbeInit.
func (m model) sessionArgs() ProviderSessionArgs {
	args := ProviderSessionArgs{
		Cwd:                m.cwd,
		TabID:              m.id,
		Model:              m.providerModel,
		Effort:             m.providerEffort,
		SkipAllPermissions: m.skipAllPermissions,
		Worktree:           m.worktree,
		ResumeCwd:          m.resumeCwd,
		AddedDirs:          append([]string(nil), m.addedDirs...),
		ProjectMCP:         projectGitHubMCP(m.cwd),
	}
	if m.sessionMinted {
		args.NewSessionID = m.sessionID
	} else {
		args.SessionID = m.sessionID
	}
	if m.workflowRun != nil {
		args.InWorkflow = true
		args.IsWorkflowFinalStep = m.workflowRun.StepIdx == len(m.workflowRun.Workflow.Steps)-1
	}
	return args
}

// projectGitHubMCP resolves the project-level GitHub MCP credentials
// for cwd into a wire-shape descriptor, or nil when the project
// hasn't configured a token.
func projectGitHubMCP(cwd string) *issueMCPServer {
	if cwd == "" {
		return nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil
	}
	pc := loadProjectConfig(cfg, cwd)
	if pc.MCP.GitHub.Token == "" {
		return nil
	}
	return &issueMCPServer{
		Name: "github",
		URL:  githubMCPEndpointOrDefault(pc.MCP.GitHub),
		Headers: map[string]string{
			"Authorization": "Bearer " + pc.MCP.GitHub.Token,
		},
	}
}

func prepareProviderSession(args ProviderSessionArgs, worktreeName string) (ProviderSessionArgs, string, error) {
	rootCwd := args.Cwd
	if rootCwd == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return args, worktreeName, err
		}
		rootCwd = cwd
	}
	return prepareProviderSessionAt(args, worktreeName, rootCwd)
}

func prepareProviderSessionAt(args ProviderSessionArgs, worktreeName, rootCwd string) (ProviderSessionArgs, string, error) {
	if rootCwd == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return args, worktreeName, err
		}
		rootCwd = cwd
	}
	if args.Cwd == "" {
		args.Cwd = rootCwd
	}
	if args.SessionID != "" && args.ResumeCwd != "" && worktreeName == "" {
		worktreeName = worktreeNameFromCwd(args.ResumeCwd)
	}

	if worktreeName == "" && args.SessionID == "" && args.Worktree &&
		worktreeBackendAt(rootCwd) != workspaceBackendNone {
		path, name, err := createWorktreeAt(rootCwd)
		if err != nil {
			return args, worktreeName, err
		}
		args.Cwd = path
		worktreeName = name
	}

	if worktreeName != "" {
		args.Cwd = worktreePath(rootCwd, worktreeName)
		if err := ensureResumeWorktree(args.Cwd); err != nil {
			return args, worktreeName, err
		}
	}
	if err := validateExecutorCwd(args, rootCwd); err != nil {
		return args, worktreeName, err
	}
	return args, worktreeName, nil
}
