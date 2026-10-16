package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// SoftEther manages one isolated Virtual Hub per LAN Room room. vpncmd must
// manage a local SoftEther VPN Server instance with a server admin password.
type SoftEther struct {
	command       string
	adminAddress  string
	adminPassword string
	publicHost    string
	publicPort    int
}

type SoftEtherConfig struct {
	Command       string
	AdminAddress  string
	AdminPassword string
	PublicHost    string
	PublicPort    int
}

func NewSoftEther(cfg SoftEtherConfig) (*SoftEther, error) {
	if cfg.Command == "" || cfg.AdminPassword == "" || cfg.PublicHost == "" || cfg.PublicPort < 1 || cfg.PublicPort > 65535 {
		return nil, errors.New("SoftEther requires vpncmd path, admin password, public host and valid public port")
	}
	if cfg.AdminAddress == "" {
		cfg.AdminAddress = "127.0.0.1:5555"
	}
	host, _, err := net.SplitHostPort(cfg.AdminAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid SoftEther admin address: %w", err)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("SoftEther admin address must be local")
	}
	command, err := exec.LookPath(cfg.Command)
	if err != nil {
		return nil, fmt.Errorf("vpncmd not found: %w", err)
	}
	return &SoftEther{command: command, adminAddress: cfg.AdminAddress, adminPassword: cfg.AdminPassword, publicHost: cfg.PublicHost, publicPort: cfg.PublicPort}, nil
}

func (v *SoftEther) Check(ctx context.Context) error {
	return v.run(ctx, "", "HubList")
}

func userName(memberID string) string { return "u" + memberID }

func (v *SoftEther) run(ctx context.Context, hub, action string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	commandArgs := []string{v.adminAddress, "/SERVER", "/PASSWORD:" + v.adminPassword}
	if hub != "" {
		commandArgs = append(commandArgs, "/ADMINHUB:"+hub)
	}
	commandArgs = append(commandArgs, "/CMD", action)
	commandArgs = append(commandArgs, args...)
	output, err := exec.CommandContext(ctx, v.command, commandArgs...).CombinedOutput()
	// vpncmd prints entered commands, including passwords. Never return its output.
	if err != nil {
		return fmt.Errorf("vpncmd %s failed: %w", action, err)
	}
	result := string(output)
	if !strings.Contains(result, "The command completed successfully.") && !strings.Contains(result, "The command terminated normally.") {
		return fmt.Errorf("vpncmd %s did not report success", action)
	}
	return nil
}

func (v *SoftEther) CreateRoom(ctx context.Context, hub, memberID, prefix string) (_ *VPNInfo, err error) {
	hubPassword, err := randomToken()
	if err != nil {
		return nil, err
	}
	if err = v.run(ctx, "", "HubCreate", hub, "/PASSWORD:"+hubPassword); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = v.DeleteRoom(context.Background(), hub)
		}
	}()
	steps := []struct {
		action string
		args   []string
	}{
		{"SetEnumDeny", nil},
		{"SecureNatHostSet", []string{"/MAC:none", "/IP:" + prefix + ".1", "/MASK:255.255.255.0"}},
		{"DhcpSet", []string{"/START:" + prefix + ".10", "/END:" + prefix + ".240", "/MASK:255.255.255.0", "/EXPIRE:7200", "/GW:none", "/DNS:none", "/DNS2:none", "/DOMAIN:none"}},
		{"DhcpEnable", nil},
		{"SecureNatEnable", nil},
		{"NatDisable", nil},
	}
	for _, step := range steps {
		if err = v.run(ctx, hub, step.action, step.args...); err != nil {
			return nil, err
		}
	}
	return v.AddMember(ctx, hub, memberID, prefix+".0/24")
}

func (v *SoftEther) AddMember(ctx context.Context, hub, memberID, subnet string) (*VPNInfo, error) {
	username := userName(memberID)
	password, err := randomToken()
	if err != nil {
		return nil, err
	}
	if err := v.run(ctx, hub, "UserCreate", username, "/GROUP:none", "/REALNAME:none", "/NOTE:none"); err != nil {
		return nil, err
	}
	if err := v.run(ctx, hub, "UserPasswordSet", username, "/PASSWORD:"+password); err != nil {
		_ = v.DeleteMember(context.Background(), hub, memberID)
		return nil, err
	}
	return &VPNInfo{Server: v.publicHost, Port: v.publicPort, Hub: hub, Username: username, Password: password, Subnet: subnet}, nil
}

func (v *SoftEther) DeleteMember(ctx context.Context, hub, memberID string) error {
	return v.run(ctx, hub, "UserDelete", userName(memberID))
}

func (v *SoftEther) DeleteRoom(ctx context.Context, hub string) error {
	return v.run(ctx, "", "HubDelete", hub)
}
