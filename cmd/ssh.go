package cmd

import (
	"context"
	"fmt"
	"github.com/gen2brain/beeep"
	"github.com/loft-sh/devpod/cmd/flags"
	"github.com/loft-sh/devpod/pkg/agent"
	client2 "github.com/loft-sh/devpod/pkg/client"
	"github.com/loft-sh/devpod/pkg/config"
	"github.com/loft-sh/devpod/pkg/log"
	provider2 "github.com/loft-sh/devpod/pkg/provider"
	devssh "github.com/loft-sh/devpod/pkg/ssh"
	"github.com/loft-sh/devpod/pkg/token"
	"github.com/loft-sh/devpod/pkg/tunnel"
	workspace2 "github.com/loft-sh/devpod/pkg/workspace"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"os"
	"os/exec"
	"time"
)

// SSHCmd holds the ssh cmd flags
type SSHCmd struct {
	*flags.GlobalFlags

	Stdio         bool
	JumpContainer bool

	Self      bool
	Configure bool
	ID        string
	User      string
}

// NewSSHCmd creates a new ssh command
func NewSSHCmd(flags *flags.GlobalFlags) *cobra.Command {
	cmd := &SSHCmd{
		GlobalFlags: flags,
	}
	sshCmd := &cobra.Command{
		Use:   "ssh",
		Short: "Starts a new ssh session to a workspace",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			devPodConfig, err := config.LoadConfig(cmd.Context)
			if err != nil {
				return err
			}

			var (
				client client2.WorkspaceClient
			)
			if cmd.Self {
				client, err = workspace2.ResolveWorkspace(ctx, devPodConfig, nil, []string{"."}, cmd.ID, cmd.Provider, log.Default)
				if err != nil {
					return err
				}
			} else {
				client, err = workspace2.GetWorkspace(ctx, devPodConfig, nil, []string{cmd.ID}, log.Default)
				if err != nil {
					return err
				}
			}

			return cmd.Run(ctx, client)
		},
	}

	sshCmd.Flags().StringVar(&cmd.ID, "id", "", "The id of the workspace to use")
	sshCmd.Flags().StringVar(&cmd.User, "user", "", "The user of the workspace to use")
	sshCmd.Flags().BoolVar(&cmd.Configure, "configure", false, "If true will configure ssh for the given workspace")
	sshCmd.Flags().BoolVar(&cmd.Stdio, "stdio", false, "If true will tunnel connection through stdout and stdin")
	sshCmd.Flags().BoolVar(&cmd.Self, "self", false, "For testing only")
	_ = sshCmd.MarkFlagRequired("id")
	_ = sshCmd.Flags().MarkHidden("self")
	return sshCmd
}

// Run runs the command logic
func (cmd *SSHCmd) Run(ctx context.Context, client client2.WorkspaceClient) error {
	if cmd.Configure {
		return configureSSH(client, "root")
	}
	if cmd.Self {
		return configureSSHSelf(client, log.Default)
	}
	if cmd.Stdio {
		return cmd.jumpContainer(ctx, client, log.Default.ErrorStreamOnly())
	}

	// TODO: Implement regular ssh client here
	return nil
}

func startWait(ctx context.Context, client client2.WorkspaceClient, create, showNotification bool, log log.Logger) error {
	startWaiting := time.Now()
	for {
		instanceStatus, err := client.Status(ctx, client2.StatusOptions{})
		if err != nil {
			return err
		} else if instanceStatus == client2.StatusBusy {
			if time.Since(startWaiting) > time.Second*10 {
				log.Infof("Waiting for instance to come up...")
				log.Debugf("Got status %s, expected: Running", instanceStatus)
				startWaiting = time.Now()
			}

			time.Sleep(time.Second)
			continue
		} else if instanceStatus == client2.StatusStopped {
			if showNotification {
				go func() {
					_ = beeep.Notify("DevPod", fmt.Sprintf("Starting currently stopped workspace %s...", client.Workspace()), "assets/information.png")
				}()
			}

			err = client.Start(ctx, client2.StartOptions{})
			if err != nil {
				return errors.Wrap(err, "start instance")
			}
		} else if instanceStatus == client2.StatusNotFound {
			if create {
				// create environment
				err = client.Create(ctx, client2.CreateOptions{})
				if err != nil {
					return err
				}
			} else {
				return fmt.Errorf("instance wasn't found")
			}
		}

		return nil
	}
}

func (cmd *SSHCmd) jumpContainer(ctx context.Context, client client2.WorkspaceClient, log log.Logger) error {
	agentClient, ok := client.(client2.AgentClient)
	if ok {
		return cmd.jumpContainerServer(ctx, agentClient, log)
	}

	if client.ProviderType() == provider2.ProviderTypeDirect {
		return cmd.jumpContainerWorkspace(ctx, client)
	}

	return nil
}

func (cmd *SSHCmd) jumpContainerWorkspace(ctx context.Context, client client2.WorkspaceClient) error {
	err := startWait(ctx, client, false, true, log.Default)
	if err != nil {
		return err
	}

	err = client.Command(ctx, client2.CommandOptions{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		return err
	}

	return nil
}

func (cmd *SSHCmd) jumpContainerServer(ctx context.Context, client client2.AgentClient, log log.Logger) error {
	err := startWait(ctx, client, false, true, log)
	if err != nil {
		return err
	}

	// get token
	tok, err := token.GenerateWorkspaceToken(client.Context(), client.Workspace())
	if err != nil {
		return err
	}

	// compute workspace info
	workspaceInfo, err := client.AgentInfo()
	if err != nil {
		return err
	}

	// create credential helper in workspace
	var runInContainer tunnel.Handler
	if client.WorkspaceConfig().IDE.IDE != provider2.IDEVSCode {
		runInContainer = func(client *ssh.Client) error {
			err := runCredentialsServer(ctx, client, cmd.User, log)
			if err != nil {
				log.Errorf("Error running credential server: %v", err)
			}

			<-ctx.Done()
			return nil
		}
	}

	// tunnel to container
	return tunnel.NewContainerTunnel(client, log).Run(ctx, func(sshClient *ssh.Client) error {
		return devssh.Run(sshClient, fmt.Sprintf("%s agent container-tunnel --start-container --token '%s' --workspace-info '%s'", client.AgentPath(), tok, workspaceInfo), os.Stdin, os.Stdout, os.Stderr)
	}, runInContainer)
}

func runCredentialsServer(ctx context.Context, client *ssh.Client, user string, log log.Logger) error {
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	defer stdoutWriter.Close()

	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	defer stdinWriter.Close()

	// start server on stdio
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errChan := make(chan error, 1)
	go func() {
		defer cancel()
		writer := log.ErrorStreamOnly().Writer(logrus.DebugLevel, false)
		defer writer.Close()

		command := fmt.Sprintf("%s agent container credentials-server --user %s", agent.RemoteDevPodHelperLocation, user)
		errChan <- devssh.Run(client, command, stdinReader, stdoutWriter, writer)
	}()

	_, err = agent.RunTunnelServer(cancelCtx, stdoutReader, stdinWriter, false, true, false, nil, log)
	if err != nil {
		return errors.Wrap(err, "run tunnel server")
	}

	// wait until command finished
	return <-errChan
}

func configureSSHSelf(client client2.WorkspaceClient, log log.Logger) error {
	tok, err := token.GenerateWorkspaceToken(client.Context(), client.Workspace())
	if err != nil {
		return err
	}

	err = devssh.ConfigureSSHConfigCommand(client.Context(), client.Workspace(), "", "devpod helper ssh-server --stdio --token "+tok, log)
	if err != nil {
		return err
	}

	err = exec.Command("code", "--folder-uri", fmt.Sprintf("vscode-remote://ssh-remote+%s.devpod/", client.Workspace())).Run()
	if err != nil {
		return err
	}
	return nil
}

func configureSSH(client client2.WorkspaceClient, user string) error {
	err := devssh.ConfigureSSHConfig(client.Context(), client.Workspace(), user, log.Default)
	if err != nil {
		return err
	}

	return nil
}