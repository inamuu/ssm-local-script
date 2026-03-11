package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

const pollInterval = 2 * time.Second

type options struct {
	profile      string
	region       string
	instanceID   string
	command      string
	commandArgs  []string
	scriptPath   string
	scriptArgs   []string
	documentName string
	timeout      time.Duration
	workDir      string
}

type instance struct {
	ID        string
	PrivateIP string
	Name      string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	opts, err := parseArgs(args)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	cfgOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithSharedConfigProfile(opts.profile),
	}
	if opts.region != "" {
		cfgOpts = append(cfgOpts, awsconfig.WithRegion(opts.region))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		return fmt.Errorf("load aws config: %w", err)
	}

	ec2Client := ec2.NewFromConfig(cfg)
	ssmClient := ssm.NewFromConfig(cfg)

	instanceID := opts.instanceID
	if instanceID == "" {
		selected, err := chooseInstance(ctx, ec2Client, ssmClient)
		if err != nil {
			return err
		}
		instanceID = selected
	}

	commands, displayName, err := buildCommands(opts)
	if err != nil {
		return err
	}

	commandID, err := sendCommand(ctx, ssmClient, instanceID, opts.documentName, opts.workDir, commands)
	if err != nil {
		return err
	}

	invocation, err := waitForInvocation(ctx, ssmClient, commandID, instanceID)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "instance: %s\n", instanceID)
	fmt.Fprintf(os.Stderr, "source: %s\n", displayName)
	fmt.Fprintf(os.Stderr, "status: %s\n", aws.ToString(invocation.StatusDetails))

	if out := strings.TrimRight(aws.ToString(invocation.StandardOutputContent), "\n"); out != "" {
		fmt.Println(out)
	}

	if errOut := strings.TrimRight(aws.ToString(invocation.StandardErrorContent), "\n"); errOut != "" {
		fmt.Fprintln(os.Stderr, errOut)
	}

	if invocation.Status != ssmtypes.CommandInvocationStatusSuccess {
		return fmt.Errorf("remote command finished with status %s", aws.ToString(invocation.StatusDetails))
	}

	return nil
}

func parseArgs(args []string) (options, error) {
	fs := flag.NewFlagSet("ssm-local-script", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	opts := options{}
	fs.StringVar(&opts.profile, "profile", "default", "AWS shared config profile")
	fs.StringVar(&opts.region, "region", "", "AWS region")
	fs.StringVar(&opts.instanceID, "instance", "", "target EC2 instance ID; if omitted fzf is used")
	fs.StringVar(&opts.command, "command", "", "shell command to run on the instance")
	fs.StringVar(&opts.scriptPath, "script", "", "local script file to run on the instance")
	fs.StringVar(&opts.documentName, "document", "AWS-RunShellScript", "SSM document name")
	fs.DurationVar(&opts.timeout, "timeout", 10*time.Minute, "overall timeout")
	fs.StringVar(&opts.workDir, "workdir", "", "remote working directory")

	if err := fs.Parse(args); err != nil {
		return opts, usageError(fs)
	}

	if (opts.command == "") == (opts.scriptPath == "") {
		return opts, fmt.Errorf("specify exactly one of -command or -script\n\n%s", usageText(fs))
	}

	if opts.command != "" {
		opts.commandArgs = fs.Args()
		if info, err := os.Stat(opts.command); err == nil && !info.IsDir() {
			opts.scriptPath = opts.command
			opts.scriptArgs = opts.commandArgs
			opts.command = ""
			opts.commandArgs = nil
		}
	}

	if opts.scriptPath != "" {
		if _, err := os.Stat(opts.scriptPath); err != nil {
			return opts, fmt.Errorf("stat script: %w", err)
		}
		if len(opts.scriptArgs) == 0 {
			opts.scriptArgs = fs.Args()
		}
	}

	return opts, nil
}

func usageError(fs *flag.FlagSet) error {
	return errors.New(usageText(fs))
}

func usageText(fs *flag.FlagSet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Usage:\n")
	fmt.Fprintf(&b, "  %s -command 'uname -a'\n", fs.Name())
	fmt.Fprintf(&b, "  %s -script ./deploy.sh -- --dry-run\n\n", fs.Name())
	fmt.Fprintf(&b, "Flags:\n")
	fs.SetOutput(&b)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
	return b.String()
}

func chooseInstance(ctx context.Context, ec2Client *ec2.Client, ssmClient *ssm.Client) (string, error) {
	instances, err := listInstances(ctx, ec2Client, ssmClient)
	if err != nil {
		return "", err
	}
	if len(instances) == 0 {
		return "", errors.New("no online SSM-managed EC2 instances found")
	}

	var input strings.Builder
	for _, inst := range instances {
		fmt.Fprintf(&input, "%s\t%s\t%s\n", inst.ID, emptyDash(inst.PrivateIP), emptyDash(inst.Name))
	}

	cmd := exec.CommandContext(ctx, "fzf", "--prompt", "Select EC2: ")
	cmd.Stdin = strings.NewReader(input.String())
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("fzf not found in PATH")
		}
		return "", fmt.Errorf("select instance with fzf: %w", err)
	}

	selected := strings.TrimSpace(out.String())
	if selected == "" {
		return "", errors.New("no instance selected")
	}

	fields := strings.Split(selected, "\t")
	if len(fields) == 0 || fields[0] == "" {
		return "", fmt.Errorf("unexpected fzf output: %q", selected)
	}

	return fields[0], nil
}

func listInstances(ctx context.Context, ec2Client *ec2.Client, ssmClient *ssm.Client) ([]instance, error) {
	instanceIDs, err := listManagedInstanceIDs(ctx, ssmClient)
	if err != nil {
		return nil, err
	}
	if len(instanceIDs) == 0 {
		return nil, nil
	}

	output, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("describe instances: %w", err)
	}

	var instances []instance
	for _, reservation := range output.Reservations {
		for _, inst := range reservation.Instances {
			instances = append(instances, instance{
				ID:        aws.ToString(inst.InstanceId),
				PrivateIP: aws.ToString(inst.PrivateIpAddress),
				Name:      tagValue(inst.Tags, "Name"),
			})
		}
	}
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].ID < instances[j].ID
	})

	return instances, nil
}

func listManagedInstanceIDs(ctx context.Context, client *ssm.Client) ([]string, error) {
	p := ssm.NewDescribeInstanceInformationPaginator(client, &ssm.DescribeInstanceInformationInput{
		Filters: []ssmtypes.InstanceInformationStringFilter{
			{
				Key:    aws.String("PingStatus"),
				Values: []string{"Online"},
			},
			{
				Key:    aws.String("ResourceType"),
				Values: []string{"EC2Instance"},
			},
		},
	})

	var instanceIDs []string
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describe managed instances: %w", err)
		}
		for _, info := range page.InstanceInformationList {
			if id := aws.ToString(info.InstanceId); id != "" {
				instanceIDs = append(instanceIDs, id)
			}
		}
	}

	return instanceIDs, nil
}

func buildCommands(opts options) ([]string, string, error) {
	if opts.command != "" {
		command := opts.command
		for _, arg := range opts.commandArgs {
			command += " " + shellQuote(arg)
		}
		return []string{command}, "inline command", nil
	}

	content, err := os.ReadFile(opts.scriptPath)
	if err != nil {
		return nil, "", fmt.Errorf("read script: %w", err)
	}

	scriptName := filepath.Base(opts.scriptPath)
	remotePath := "/tmp/" + scriptName
	var b strings.Builder
	fmt.Fprintf(&b, "cat <<'__SSM_LOCAL_SCRIPT__' > %s\n", shellQuote(remotePath))
	b.Write(content)
	if len(content) == 0 || content[len(content)-1] != '\n' {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "__SSM_LOCAL_SCRIPT__\nchmod +x %s\n", shellQuote(remotePath))
	fmt.Fprintf(&b, "%s", shellQuote(remotePath))
	for _, arg := range opts.scriptArgs {
		fmt.Fprintf(&b, " %s", shellQuote(arg))
	}
	b.WriteByte('\n')

	return []string{b.String()}, opts.scriptPath, nil
}

func sendCommand(ctx context.Context, client *ssm.Client, instanceID, documentName, workDir string, commands []string) (string, error) {
	params := map[string][]string{
		"commands": commands,
	}
	if workDir != "" {
		params["workingDirectory"] = []string{workDir}
	}

	output, err := client.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName: aws.String(documentName),
		InstanceIds:  []string{instanceID},
		Parameters:   params,
	})
	if err != nil {
		return "", fmt.Errorf("send command: %w", err)
	}

	return aws.ToString(output.Command.CommandId), nil
}

func waitForInvocation(ctx context.Context, client *ssm.Client, commandID, instanceID string) (*ssm.GetCommandInvocationOutput, error) {
	waiter := ssm.NewCommandExecutedWaiter(client, func(o *ssm.CommandExecutedWaiterOptions) {
		o.MinDelay = pollInterval
		o.MaxDelay = 10 * time.Second
	})

	input := &ssm.GetCommandInvocationInput{
		CommandId:  aws.String(commandID),
		InstanceId: aws.String(instanceID),
	}

	waitErr := waiter.Wait(ctx, input, optsTimeoutRemaining(ctx))
	output, getErr := client.GetCommandInvocation(ctx, input)
	if getErr != nil {
		if waitErr != nil {
			return nil, fmt.Errorf("wait for command result: %w (get invocation: %v)", waitErr, getErr)
		}
		return nil, fmt.Errorf("get command invocation: %w", getErr)
	}

	if waitErr != nil && output.Status == ssmtypes.CommandInvocationStatusInProgress {
		return nil, fmt.Errorf("wait for command result: %w", waitErr)
	}

	return output, nil
}

func tagValue(tags []ec2types.Tag, key string) string {
	for _, tag := range tags {
		if aws.ToString(tag.Key) == key {
			return aws.ToString(tag.Value)
		}
	}
	return ""
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func optsTimeoutRemaining(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 {
			return remaining
		}
	}
	return time.Second
}
