package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	version "github.com/hashicorp/go-version"
	"github.com/pkg/errors"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
)

const (
	defaultWorkspace = "default"
	refreshKeyword   = "Refreshing state..."
	refreshSeparator = "------------------------------------------------------------------------\n"
)

var (
	plusDiffRegex  = regexp.MustCompile(`(?m)^ {2}\+`)
	tildeDiffRegex = regexp.MustCompile(`(?m)^ {2}~`)
	minusDiffRegex = regexp.MustCompile(`(?m)^ {2}-`)
)

type planStepRunner struct {
	TerraformExecutor   TerraformExec
	DefaultTFVersion    *version.Version
	CommitStatusUpdater StatusUpdater
	AsyncTFExec         AsyncTFExec
}

func NewPlanStepRunner(terraformExecutor TerraformExec, defaultTfVersion *version.Version, commitStatusUpdater StatusUpdater, asyncTFExec AsyncTFExec) Runner {
	runner := &planStepRunner{
		TerraformExecutor:   terraformExecutor,
		DefaultTFVersion:    defaultTfVersion,
		CommitStatusUpdater: commitStatusUpdater,
		AsyncTFExec:         asyncTFExec,
	}
	return NewWorkspaceStepRunnerDelegate(terraformExecutor, defaultTfVersion, runner)
}

func (p *planStepRunner) Run(ctx command.ProjectContext, extraArgs []string, path string, envs map[string]string) (string, error) {
	tfVersion := p.DefaultTFVersion
	if ctx.TerraformVersion != nil {
		tfVersion = ctx.TerraformVersion
	}

	planFile := filepath.Join(path, GetPlanFilename(ctx.Workspace, ctx.ProjectName))
	planCmd := p.buildPlanCmd(ctx, extraArgs, path, tfVersion, planFile)
	output, err := p.TerraformExecutor.RunCommandWithVersion(ctx, filepath.Clean(path), planCmd, envs, tfVersion, ctx.Workspace)
	if p.isRemoteOpsErr(output, err) {
		ctx.Log.Debug("detected that this project is using TFE remote ops")
		return p.remotePlan(ctx, extraArgs, path, tfVersion, planFile, envs)
	}
	if err != nil {
		return output, err
	}
	return p.fmtPlanOutput(output, tfVersion), nil
}

// isRemoteOpsErr returns true if there was an error caused due to this
// project using TFE remote operations.
func (p *planStepRunner) isRemoteOpsErr(output string, err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(output, remoteOpsErr110) || strings.Contains(output, remoteOpsErr01114) || strings.Contains(output, remoteOpsErr012) || strings.Contains(output, remoteOpsErr100)
}

// remotePlan runs a terraform plan command compatible with TFE remote
// operations.
func (p *planStepRunner) remotePlan(ctx command.ProjectContext, extraArgs []string, path string, tfVersion *version.Version, planFile string, envs map[string]string) (string, error) {
	argList := [][]string{
		{"plan", "-input=false", "-refresh", "-no-color"},
		extraArgs,
		ctx.EscapedCommentArgs,
	}
	args := p.flatten(argList)
	output, err := p.runRemotePlan(ctx, args, path, tfVersion, envs)
	if err != nil {
		return output, err
	}

	// If using remote ops, we create our own "fake" planfile with the
	// text output of the plan. We do this for two reasons:
	// 1) Atlantis relies on there being a planfile on disk to detect which
	// projects have outstanding plans.
	// 2) Remote ops don't support the -out parameter so we can't save the
	// plan. To ensure that what gets applied is the plan we printed to the PR,
	// during the apply phase, we diff the output we stored in the fake
	// planfile with the pending apply output.
	planOutput := StripRefreshingFromPlanOutput(output, tfVersion)

	// We also prepend our own remote ops header to the file so during apply we
	// know this is a remote apply.
	err = os.WriteFile(planFile, []byte(remoteOpsHeader+planOutput), 0600)
	if err != nil {
		return output, errors.Wrap(err, "unable to create planfile for remote ops")
	}

	return p.fmtPlanOutput(output, tfVersion), nil
}

func (p *planStepRunner) buildPlanCmd(ctx command.ProjectContext, extraArgs []string, path string, tfVersion *version.Version, planFile string) []string {
	tfVars := p.tfVars(ctx, tfVersion)

	// Check if env/{workspace}.tfvars exist and include it. This is a use-case
	// from Hootsuite where Atlantis was first created so we're keeping this as
	// an homage and a favor so they don't need to refactor all their repos.
	// It's also a nice way to structure your repos to reduce duplication.
	var envFileArgs []string
	envFile := filepath.Join(path, "env", ctx.Workspace+".tfvars")
	if _, err := os.Stat(envFile); err == nil {
		envFileArgs = []string{"-var-file", envFile}
	}

	argList := [][]string{
		// NOTE: we need to quote the plan filename because Bitbucket Server can
		// have spaces in its repo owner names.
		{"plan", "-input=false", "-refresh", "-out", fmt.Sprintf("%q", planFile)},
		tfVars,
		extraArgs,
		ctx.EscapedCommentArgs,
		envFileArgs,
	}

	return p.flatten(argList)
}

// tfVars returns a list of "-var", "key=value" pairs that identify who and which
// repo this command is running for. This can be used for naming the
// session name in AWS which will identify in CloudTrail the source of
// Atlantis API calls.
// If using Terraform >= 0.12 we don't set any of these variables because
// those versions don't allow setting -var flags for any variables that aren't
// actually used in the configuration. Since there's no way for us to detect
// if the configuration is using those variables, we don't set them.
func (p *planStepRunner) tfVars(ctx command.ProjectContext, tfVersion *version.Version) []string {
	if tfVersion.GreaterThanOrEqual(version.Must(version.NewVersion("0.12.0"))) {
		return nil
	}

	// NOTE: not using maps and looping here because we need to keep the
	// ordering for testing purposes.
	// NOTE: quoting the values because in Bitbucket the owner can have
	// spaces, ex -var atlantis_repo_owner="bitbucket owner".
	return []string{
		"-var",
		fmt.Sprintf("%s=%q", "atlantis_user", ctx.User.Username),
		"-var",
		fmt.Sprintf("%s=%q", "atlantis_repo", ctx.BaseRepo.FullName),
		"-var",
		fmt.Sprintf("%s=%q", "atlantis_repo_name", ctx.BaseRepo.Name),
		"-var",
		fmt.Sprintf("%s=%q", "atlantis_repo_owner", ctx.BaseRepo.Owner),
		"-var",
		fmt.Sprintf("%s=%d", "atlantis_pull_num", ctx.Pull.Num),
	}
}

func (p *planStepRunner) flatten(slices [][]string) []string {
	var flattened []string
	for _, v := range slices {
		flattened = append(flattened, v...)
	}
	return flattened
}

// fmtPlanOutput uses regex's to remove any leading whitespace in front of the
// terraform output so that the diff syntax highlighting works. Example:
// "  - aws_security_group_rule.allow_all" =>
// "- aws_security_group_rule.allow_all"
// We do it for +, ~ and -.
// It also removes the "Refreshing..." preamble.
func (p *planStepRunner) fmtPlanOutput(output string, tfVersion *version.Version) string {
	output = StripRefreshingFromPlanOutput(output, tfVersion)
	output = plusDiffRegex.ReplaceAllString(output, "+")
	output = tildeDiffRegex.ReplaceAllString(output, "~")
	return minusDiffRegex.ReplaceAllString(output, "-")
}

// runRemotePlan runs a terraform command that utilizes the remote operations
// backend. It watches the command output for the run url to be printed, and
// then updates the commit status with a link to the run url.
// The run url is a link to the Terraform Enterprise UI where the output
// from the in-progress command can be viewed.
// cmdArgs is the args to terraform to execute.
// path is the path to where we need to execute.
func (p *planStepRunner) runRemotePlan(
	ctx command.ProjectContext,
	cmdArgs []string,
	path string,
	tfVersion *version.Version,
	envs map[string]string) (string, error) {

	// updateStatusF will update the commit status and log any error.
	updateStatusF := func(status models.CommitStatus, url string) {
		if err := p.CommitStatusUpdater.UpdateProject(ctx, command.Plan, status, url, nil); err != nil {
			ctx.Log.Err("unable to update status: %s", err)
		}
	}

	// Start the async command execution.
	ctx.Log.Debug("starting async tf remote operation")
	_, outCh := p.AsyncTFExec.RunCommandAsync(ctx, filepath.Clean(path), cmdArgs, envs, tfVersion, ctx.Workspace)
	var lines []string
	nextLineIsRunURL := false
	var runURL string
	var err error

	for line := range outCh {
		if line.Err != nil {
			err = line.Err
			break
		}
		lines = append(lines, line.Line)

		// Here we're checking for the run url and updating the status
		// if found.
		if line.Line == lineBeforeRunURL {
			nextLineIsRunURL = true
		} else if nextLineIsRunURL {
			runURL = strings.TrimSpace(line.Line)
			ctx.Log.Debug("remote run url found, updating commit status")
			updateStatusF(models.PendingCommitStatus, runURL)
			nextLineIsRunURL = false
		}
	}

	ctx.Log.Debug("async tf remote operation complete")
	output := strings.Join(lines, "\n")
	if err != nil {
		updateStatusF(models.FailedCommitStatus, runURL)
	} else {
		updateStatusF(models.SuccessCommitStatus, runURL)
	}
	return output, err
}

func StripRefreshingFromPlanOutput(output string, tfVersion *version.Version) string {
	if tfVersion.GreaterThanOrEqual(version.Must(version.NewVersion("0.14.0"))) {
		// Plan output contains a lot of "Refreshing..." lines, remove it
		lines := strings.Split(output, "\n")
		finalIndex := 0
		for i, line := range lines {
			if strings.Contains(line, refreshKeyword) {
				finalIndex = i
			}
		}

		if finalIndex != 0 {
			output = strings.Join(lines[finalIndex+1:], "\n")
		}
	} else {
		// Plan output contains a lot of "Refreshing..." lines followed by a
		// separator. We want to remove everything before that separator.
		sepIdx := strings.Index(output, refreshSeparator)
		if sepIdx > -1 {
			output = output[sepIdx+len(refreshSeparator):]
		}
	}
	return output
}

// remoteOpsErr01114 is the error terraform plan will return if this project is
// using TFE remote operations in TF 0.11.15.
var remoteOpsErr01114 = `Error: Saving a generated plan is currently not supported!

The "remote" backend does not support saving the generated execution
plan locally at this time.

`

// remoteOpsErr012 is the error terraform plan will return if this project is
// using TFE remote operations in TF 0.12.{0-4}. Later versions haven't been
// released yet at this time.
var remoteOpsErr012 = `Error: Saving a generated plan is currently not supported

The "remote" backend does not support saving the generated execution plan
locally at this time.

`

// remoteOpsErr100 is the error terraform plan will retrun if this project is
// using TFE remote operations in TF 1.0.{0,1}.
var remoteOpsErr100 = `Error: Saving a generated plan is currently not supported

The "remote" backend does not support saving the generated execution plan
locally at this time.
`

// remoteOpsErr110 is the error terraform plan will return if this project is
// using Terraform Cloud remote operations in TF 1.1.0 and above
var remoteOpsErr110 = `╷
│ Error: Saving a generated plan is currently not supported
│
│ Terraform Cloud does not support saving the generated execution plan
│ locally at this time.
╵
`

// remoteOpsHeader is the header we add to the planfile if this plan was
// generated using TFE remote operations.
var remoteOpsHeader = "Atlantis: this plan was created by remote ops\n"
