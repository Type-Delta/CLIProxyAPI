package cmd

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	log "github.com/sirupsen/logrus"
)

// DoKiroLogin triggers the Kiro IAM SSO OAuth flow and saves credentials.
func DoKiroLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}
	if options.Prompt == nil {
		options.Prompt = defaultProjectPrompt()
	}
	manager := newAuthManager()
	authOpts := &sdkAuth.LoginOptions{NoBrowser: options.NoBrowser, CallbackPort: options.CallbackPort, Metadata: map[string]string{}, Prompt: options.Prompt}
	record, savedPath, err := manager.Login(context.Background(), "kiro", cfg, authOpts)
	if err != nil {
		log.Errorf("Kiro authentication failed: %v", err)
		return
	}
	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	fmt.Println("Kiro authentication successful!")
}
