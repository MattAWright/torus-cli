package cmd

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/manifoldco/go-base32"
	"github.com/manifoldco/go-base64"
	"github.com/urfave/cli"
	"github.com/juju/ansiterm"

	"github.com/manifoldco/torus-cli/api"
	"github.com/manifoldco/torus-cli/apitypes"
	"github.com/manifoldco/torus-cli/config"
	"github.com/manifoldco/torus-cli/envelope"
	"github.com/manifoldco/torus-cli/errs"
	"github.com/manifoldco/torus-cli/gatekeeper/bootstrap"
	"github.com/manifoldco/torus-cli/hints"
	"github.com/manifoldco/torus-cli/identity"
	"github.com/manifoldco/torus-cli/primitive"
	"github.com/manifoldco/torus-cli/ui"
)

const (
	machineRandomIDLength = 5 // 8 characters in base32
	machineCreateFailed   = "Could not create machine, please try again."

	// GlobalRoot is the global root of the Torus config
	GlobalRoot = "/etc/torus"

	// EnvironmentFile is the environment file that stores machine information
	EnvironmentFile = "token.environment"
)

// urlFlag creates a new --bootstrap cli.Flag
func urlFlag(usage string, required bool) cli.Flag {
	return newPlaceholder("url, u", "URL", usage, "", "TORUS_BOOTSTRAP_URL", required)
}

// authProviderFlag creates a new --auth cli.Flag
func authProviderFlag(usage string, required bool) cli.Flag {
	return newPlaceholder("auth, a", "AUTHPROVIDER", usage, "", "TORUS_AUTH_PROVIDER", required)
}

func caFlag(usage string, required bool) cli.Flag {
	return newPlaceholder("ca", "CA_BUNDLE", usage, "", "TORUS_BOOTSTRAP_CA", required)
}

func init() {
	machines := cli.Command{
		Name:      "machines",
		Usage:     "View and create machines within an organization",
		ArgsUsage: "<machine>",
		Category:  "ORGANIZATIONS",
		Subcommands: []cli.Command{
			{
				Name:  "create",
				Usage: "Create a machine for an organization",
				Flags: []cli.Flag{
					orgFlag("Org the machine will belong to", false),
					roleFlag("Role the machine will belong to", false),
				},
				Action: chain(
					ensureDaemon, ensureSession, loadDirPrefs, loadPrefDefaults,
					checkRequiredFlags, createMachine,
				),
			},
			{
				Name:  "list",
				Usage: "List machines for an organization",
				Flags: []cli.Flag{
					orgFlag("Org the machine belongs to", false),
					roleFlag("List machines of this role", false),
					destroyedFlag(),
				},
				Action: chain(
					ensureDaemon, ensureSession, loadDirPrefs, loadPrefDefaults,
					checkRequiredFlags, listMachinesCmd,
				),
			},
			{
				Name:      "view",
				Usage:     "Show the details of a machine",
				ArgsUsage: "<id|name>",
				Flags: []cli.Flag{
					orgFlag("Org the machine will belongs to", false),
				},
				Action: chain(
					ensureDaemon, ensureSession, loadDirPrefs, loadPrefDefaults,
					checkRequiredFlags, viewMachineCmd,
				),
			},
			{
				Name:      "destroy",
				Usage:     "Destroy a machine in the specified organization",
				ArgsUsage: "<id|name>",
				Flags: []cli.Flag{
					orgFlag("Org the machine will belongs to", true),
					stdAutoAcceptFlag,
				},
				Action: chain(
					ensureDaemon, ensureSession, loadDirPrefs, loadPrefDefaults,
					checkRequiredFlags, destroyMachineCmd,
				),
			},
			{
				Name:      "roles",
				Usage:     "Lists and create machine roles for an organization",
				ArgsUsage: "<machine-role>",
				Subcommands: []cli.Command{
					{
						Name:      "create",
						Usage:     "Create a machine role for an organization",
						ArgsUsage: "<name>",
						Flags: []cli.Flag{
							orgFlag("Org the machine role will belong to", true),
						},
						Action: chain(
							ensureDaemon, ensureSession, loadDirPrefs, loadPrefDefaults,
							checkRequiredFlags, createMachineRole,
						),
					},
					{
						Name:  "list",
						Usage: "List all machine roles for an organization",
						Flags: []cli.Flag{
							orgFlag("Org the machine roles belongs to", true),
						},
						Action: chain(
							ensureDaemon, ensureSession, loadDirPrefs, loadPrefDefaults,
							checkRequiredFlags, listMachineRoles,
						),
					},
				},
			},
			{
				Name:  "bootstrap",
				Usage: "Bootstrap a new machine using Torus Gatekeeper",
				Flags: []cli.Flag{
					authProviderFlag("Auth provider for bootstrapping", true),
					urlFlag("Gatekeeper URL for bootstrapping", true),
					roleFlag("Role the machine will belong to", true),
					machineFlag("Machine name to bootstrap", false),
					orgFlag("Org the machine will belong to", false),
					caFlag("CA Bundle to use for certificate verification. Uses system if none is provided", false),
				},
				Action: chain(checkRequiredFlags, bootstrapCmd),
			},
		},
	}
	Cmds = append(Cmds, machines)
}

func destroyMachineCmd(ctx *cli.Context) error {
	args := ctx.Args()
	if len(args) > 1 {
		return errs.NewUsageExitError("Too many arguments supplied.", ctx)
	}
	if len(args) < 1 {
		return errs.NewUsageExitError("Name or ID is required", ctx)
	}
	if ctx.String("org") == "" {
		return errs.NewUsageExitError("Missing flags: --org", ctx)
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}

	client := api.NewClient(cfg)
	c := context.Background()

	// Look up the target org
	org, err := getOrg(c, client, ctx.String("org"))
	if err != nil {
		return errs.NewErrorExitError("Machine destroy failed", err)
	}
	if org == nil {
		return errs.NewExitError("Org not found.")
	}

	machineID, err := identity.DecodeFromString(args[0])
	if err != nil {
		name := args[0]
		machines, lErr := client.Machines.List(c, org.ID, nil, &name, nil)
		if lErr != nil {
			return errs.NewErrorExitError("Failed to retrieve machine", err)
		}
		if len(machines) < 1 {
			return errs.NewExitError("Machine not found")
		}
		machineID = *machines[0].Machine.ID
	}

	preamble := "You are about to destroy a machine. This cannot be undone."
	abortErr := ConfirmDialogue(ctx, nil, &preamble, "", true)
	if abortErr != nil {
		return abortErr
	}

	err = client.Machines.Destroy(c, &machineID)
	if err != nil {
		return errs.NewErrorExitError("Failed to destroy machine", err)
	}

	fmt.Println("Machine destroyed.")
	return nil
}

func viewMachineCmd(ctx *cli.Context) error {
	args := ctx.Args()
	if len(args) > 1 {
		return errs.NewUsageExitError("Too many arguments supplied.", ctx)
	}
	if len(args) < 1 {
		return errs.NewUsageExitError("Name or ID is required", ctx)
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}

	client := api.NewClient(cfg)
	c := context.Background()

	org, err := getOrgWithPrompt(client, c, ctx.String("org"))
	if err != nil {
		return err
	}

	machineID, err := identity.DecodeFromString(args[0])
	if err != nil {
		name := args[0]
		machines, lErr := client.Machines.List(c, org.ID, nil, &name, nil)
		if lErr != nil {
			return errs.NewErrorExitError("Failed to retrieve machine", lErr)
		}
		if len(machines) < 1 {
			return errs.NewExitError("Machine not found")
		}
		machineID = *machines[0].Machine.ID
	}

	machineSegment, err := client.Machines.Get(c, &machineID)
	if err != nil {
		return errs.NewErrorExitError("Failed to retrieve machine", err)
	}
	if machineSegment == nil {
		return errs.NewExitError("Machine not found.")
	}

	orgTrees, err := client.Orgs.GetTree(c, *org.ID)
	if err != nil {
		return errs.NewErrorExitError("Failed to retrieve machine", err)
	}
	if len(orgTrees) < 1 {
		return errs.NewExitError("Machine metadata not found.")
	}
	orgTree := orgTrees[0]

	profileMap := make(map[identity.ID]apitypes.Profile, len(orgTree.Profiles))
	for _, p := range orgTree.Profiles {
		profileMap[*p.ID] = *p
	}

	teamMap := make(map[identity.ID]envelope.Team, len(orgTree.Teams))
	for _, t := range orgTree.Teams {
		teamMap[*t.Team.ID] = *t.Team
	}

	machine := machineSegment.Machine
	machineBody := machine.Body

	// Created profile
	creator := profileMap[*machineBody.CreatedBy]
	createdBy := creator.Body.Username + " (" + creator.Body.Name + ")"
	createdOn := machineBody.Created.Format(time.RFC3339)

	// Destroyed profile
	destroyedOn := "-"
	destroyedBy := "-"
	if machineBody.State == primitive.MachineDestroyedState {
		destroyer := profileMap[*machineBody.DestroyedBy]
		destroyedOn = machineBody.Destroyed.Format(time.RFC3339)
		destroyedBy = destroyer.Body.Username + " (" + destroyer.Body.Name + ")"
	}

	// Membership info
	var teamNames []string
	for _, m := range machineSegment.Memberships {
		team := teamMap[*m.Body.TeamID]
		if team.Body.TeamType == primitive.MachineTeamType {
			teamNames = append(teamNames, team.Body.Name)
		}
	}
	roleOutput := strings.Join(teamNames, ", ")
	if roleOutput == "" {
		roleOutput = "-"
	}

	fmt.Println("")
	w1 := tabwriter.NewWriter(os.Stdout, 0, 0, 8, ' ', 0)
	fmt.Fprintf(w1, "ID:\t%s\n", machine.ID)
	fmt.Fprintf(w1, "Name:\t%s\n", machineBody.Name)
	fmt.Fprintf(w1, "Role:\t%s\n", roleOutput)
	fmt.Fprintf(w1, "State:\t%s\n", machineBody.State)
	fmt.Fprintf(w1, "Created By:\t%s\n", createdBy)
	fmt.Fprintf(w1, "Created On:\t%s\n", createdOn)
	fmt.Fprintf(w1, "Destroyed By:\t%s\n", destroyedBy)
	fmt.Fprintf(w1, "Destroyed On:\t%s\n", destroyedOn)
	w1.Flush()
	fmt.Println("")

	w2 := ansiterm.NewTabWriter(os.Stdout, 2, 0, 3, ' ', 0)
	fmt.Fprintf(w2, "%s\t%s\t%s\t%s\n", ui.Bold("Token ID"), ui.Bold("State"), ui.Bold("Created By"), ui.Bold("Created On"))
	for _, token := range machineSegment.Tokens {
		tokenID := token.Token.ID
		var state string
		if token.Token.Body.State == "active" {
			state = ui.Color(ui.Green, token.Token.Body.State)
		} else {
			state = ui.Color(ui.Red, token.Token.Body.State)
		}
		creator := profileMap[*token.Token.Body.CreatedBy]
		createdBy := creator.Body.Username + " (" + creator.Body.Name + ")"
		createdOn := token.Token.Body.Created.Format(time.RFC3339)
		fmt.Fprintf(w2, "%s\t%s\t%s\t%s\n", tokenID, state, createdBy, createdOn)
	}

	w2.Flush()
	fmt.Println("")

	return nil
}

func listMachinesCmd(ctx *cli.Context) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}

	client := api.NewClient(cfg)
	c := context.Background()

	args := ctx.Args()
	if len(args) > 0 {
		return errs.NewUsageExitError("Too many arguments supplied.", ctx)
	}

	org, err := getOrgWithPrompt(client, c, ctx.String("org"))
	if err != nil {
		return err
	}

	state := primitive.MachineActiveState
	if ctx.Bool("destroyed") {
		state = primitive.MachineDestroyedState
	}

	if ctx.String("role") != "" && ctx.Bool("destroyed") {
		return errs.NewExitError(
			"Cannot specify --destroyed and --role at the same time")
	}

	roles, err := client.Teams.List(c, org.ID, ctx.String("role"), primitive.MachineTeamType)
	if err != nil {
		return errs.NewErrorExitError("Failed to retrieve metadata", err)
	}

	// If no role is given, we don't want to error for role not found when there
	// are no roles at all. instead we want to error with no machines found.
	if len(roles) < 1 && ctx.String("role") != "" {
		return errs.NewExitError("Machine role not found.")
	}

	var roleID *identity.ID
	if ctx.String("role") != "" {
		roleID = roles[0].ID
	}

	machines, err := client.Machines.List(c, org.ID, &state, nil, roleID)
	if err != nil {
		return err
	}

	if len(machines) == 0 {
		fmt.Println("No machines found.")
		return nil
	}

	roleMap := make(map[identity.ID]primitive.Team, len(roles))
	for _, t := range roles {
		if t.Body.TeamType == primitive.MachineTeamType {
			roleMap[*t.ID] = *t.Body
		}
	}

	fmt.Println("")
	w := ansiterm.NewTabWriter(os.Stdout, 2, 0, 3, ' ', 0)
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ui.Bold("ID"), ui.Bold("Name"), ui.Bold("State"), ui.Bold("Role"), ui.Bold("Creation Date"))
	for _, machine := range machines {
		mID := machine.Machine.ID.String()
		m := machine.Machine.Body
		roleName := "-"
		for _, m := range machine.Memberships {
			role, ok := roleMap[*m.Body.TeamID]
			if ok {
				roleName = role.Name
			}
		}
		var state string
		if m.State == "active" {
			state = ui.Color(ui.Green, m.State)
		} else {
			state = ui.Color(ui.Red, m.State)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", mID, m.Name, state, roleName, m.Created.Format(time.RFC3339))
	}
	w.Flush()
	fmt.Println("")

	return nil
}

// bootstrapCmd is the cli.Command for Bootstrapping machine configuration from the Gatekeeper
func bootstrapCmd(ctx *cli.Context) error {
	cloud := ctx.String("auth")

	resp, err := bootstrap.Do(
		bootstrap.Provider(cloud),
		ctx.String("url"),
		ctx.String("machine"),
		ctx.String("org"),
		ctx.String("role"),
		ctx.String("ca"),
	)
	if err != nil {
		return fmt.Errorf("bootstrap provision failed: %s", err)
	}

	envFile := filepath.Join(GlobalRoot, EnvironmentFile)
	err = writeEnvironmentFile(resp.Token, resp.Secret)
	if err != nil {
		return fmt.Errorf("failed to write environment file[%s]: %s", envFile, err)
	}

	fmt.Printf("Machine bootstrapped. Environment configuration saved in %s\n", envFile)
	return nil
}

func listMachineRoles(ctx *cli.Context) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}

	client := api.NewClient(cfg)
	c := context.Background()

	args := ctx.Args()
	if len(args) > 0 {
		return errs.NewUsageExitError("Too many arguments supplied", ctx)
	}

	org, err := client.Orgs.GetByName(c, ctx.String("org"))
	if err != nil {
		return errs.NewErrorExitError("Failed to retrieve org", err)
	}
	if org == nil {
		return errs.NewExitError("Org not found.")
	}

	teams, err := client.Teams.List(c, org.ID, "", primitive.AnyTeamType)
	if err != nil {
		return errs.NewErrorExitError("Failed to retrieve roles", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 20, 0, 1, ' ', 0)
	for _, t := range teams {
		if !isMachineTeam(t.Body) {
			continue
		}

		displayTeamType := ""
		if t.Body.TeamType == primitive.SystemTeamType && t.Body.Name == primitive.MachineTeamName {
			displayTeamType = "[system]"
		}

		fmt.Fprintf(w, "%s\t%s\n", t.Body.Name, displayTeamType)
	}

	w.Flush()
	fmt.Println("\nAll machines belong to the \"machine\" role.")
	return nil
}

func createMachineRole(ctx *cli.Context) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}

	args := ctx.Args()
	teamName := ""
	if len(args) > 0 {
		teamName = args[0]
	}

	client := api.NewClient(cfg)
	c := context.Background()

	org, oName, newOrg, err := SelectCreateOrg(c, client, ctx.String("org"))
	if err != nil {
		return handleSelectError(err, "Org selection failed")
	}
	if org == nil && !newOrg {
		fmt.Println("")
		return errs.NewExitError("Org not found.")
	}
	if newOrg && oName == "" {
		fmt.Println("")
		return errs.NewExitError("Invalid org name.")
	}

	var orgID *identity.ID
	if org != nil {
		orgID = org.ID
	}

	label := "Role name"
	autoAccept := teamName != ""
	teamName, err = NamePrompt(&label, teamName, autoAccept)
	if err != nil {
		return handleSelectError(err, "Role creation failed.")
	}

	if org == nil && newOrg {
		org, err = createOrgByName(c, ctx, client, oName)
		if err != nil {
			fmt.Println("")
			return err
		}

		orgID = org.ID
	}

	fmt.Println("")
	_, err = client.Teams.Create(c, orgID, teamName, primitive.MachineTeamType)
	if err != nil {
		if strings.Contains(err.Error(), "resource exists") {
			return errs.NewExitError("Role already exists")
		}

		return errs.NewErrorExitError("Role creation failed.", err)
	}

	fmt.Printf("Role %s created.\n", teamName)
	hints.Display(hints.Allow, hints.Deny, hints.Policies)
	return nil
}

func createMachine(ctx *cli.Context) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}

	client := api.NewClient(cfg)
	c := context.Background()

	org, orgName, newOrg, err := SelectCreateOrg(c, client, ctx.String("org"))
	if err != nil {
		return handleSelectError(err, "Org selection failed.")
	}

	var orgID *identity.ID
	if !newOrg {
		if org == nil {
			return errs.NewExitError("Org not found.")
		}
		orgID = org.ID
	}

	team, teamName, newTeam, err := SelectCreateRole(c, client, orgID, ctx.String("role"))
	if err != nil {
		return handleSelectError(err, "Role selection failed.")
	}

	var teamID *identity.ID
	if !newTeam {
		if org == nil {
			return errs.NewExitError("Role not found.")
		}
		teamID = team.ID
	}

	args := ctx.Args()
	name := ""
	if len(args) > 0 {
		name = args[0]
	}

	name, err = promptForMachineName(name, teamName)
	fmt.Println()
	if err != nil {
		return errs.NewErrorExitError(machineCreateFailed, err)
	}

	if newOrg {
		org, err := client.Orgs.Create(c, orgName)
		if err != nil {
			return errs.NewErrorExitError("Could not create org", err)
		}

		orgID = org.ID
		err = generateKeypairsForOrg(c, ctx, client, org.ID, false)
		if err != nil {
			return err
		}

		fmt.Printf("Org %s created.\n\n", orgName)
	}

	if newTeam {
		team, err := client.Teams.Create(c, orgID, teamName, primitive.MachineTeamType)
		if err != nil {
			return errs.NewErrorExitError("Could not create machine role", err)
		}

		teamID = team.ID
		fmt.Printf("Machine role %s created for org %s.\n\n", teamName, orgName)
	}

	machine, tokenSecret, err := createMachineByName(c, client, orgID, teamID, name)
	if err != nil {
		return err
	}

	fmt.Print("\nYou will only be shown the secret once, please keep it safe.\n\n")

	w := tabwriter.NewWriter(os.Stdout, 2, 0, 1, ' ', 0)

	tokenID := machine.Tokens[0].Token.ID
	fmt.Fprintf(w, "Machine ID:\t%s\n", machine.Machine.ID)
	fmt.Fprintf(w, "Machine Token ID:\t%s\n", tokenID)
	fmt.Fprintf(w, "Machine Token Secret:\t%s\n", tokenSecret)

	w.Flush()
	hints.Display(hints.Allow, hints.Deny)
	return nil
}

func createMachineByName(c context.Context, client *api.Client,
	orgID, teamID *identity.ID, name string) (*apitypes.MachineSegment, *base64.Value, error) {

	machine, tokenSecret, err := client.Machines.Create(
		c, orgID, teamID, name, progress)
	if err != nil {
		if strings.Contains(err.Error(), "resource exists") {
			return nil, nil, errs.NewExitError("Machine already exists")
		}

		return nil, nil, errs.NewErrorExitError(
			"Could not create machine, please try again.", err)
	}

	return machine, tokenSecret, nil
}

func promptForMachineName(providedName, teamName string) (string, error) {
	defaultName, err := deriveMachineName(teamName)
	if err != nil {
		return "", errs.NewErrorExitError("Could not derive machine name", err)
	}

	var name string
	if providedName == "" {
		name = defaultName
	} else {
		name = providedName
	}

	label := "Enter machine name"
	autoAccept := providedName != ""
	return NamePrompt(&label, name, autoAccept)
}

func deriveMachineName(teamName string) (string, error) {
	value := make([]byte, machineRandomIDLength)
	_, err := rand.Read(value)
	if err != nil {
		return "", err
	}

	name := teamName + "-" + base32.EncodeToString(value)
	return name, nil
}

func writeEnvironmentFile(token *identity.ID, secret *base64.Value) error {
	_, err := os.Stat(GlobalRoot)
	if os.IsNotExist(err) {
		os.Mkdir(GlobalRoot, 0700)
	}

	envPath := filepath.Join(GlobalRoot, EnvironmentFile)
	f, err := os.Create(envPath)
	if err != nil {
		return err
	}
	os.Chmod(envPath, 0600)

	w := bufio.NewWriter(f)
	w.WriteString(fmt.Sprintf("TORUS_TOKEN_ID=%s\n", token))
	w.WriteString(fmt.Sprintf("TORUS_TOKEN_SECRET=%s\n", secret))
	w.Flush()

	return nil
}
