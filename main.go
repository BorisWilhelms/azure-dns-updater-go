package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/dns/armdns"
	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"golang.org/x/exp/slog"
)

// Config holds all application configuration
type Config struct {
	Interval          time.Duration
	AzureTenantID     string
	AzureClientID     string
	AzureClientSecret string
	AzureSubID        string
	AzureResourceGroup string
	AzureDNSZone      string
	AzureDNSRecords   []string
}

// DNSUpdater manages the DNS update process
type DNSUpdater struct {
	config Config
	logger *slog.Logger
	client *armdns.RecordSetsClient
	prevIP string
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// Load configuration
	config, err := loadConfig(logger)
	if err != nil {
		logger.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	// Create Azure DNS client
	cred, err := azidentity.NewClientSecretCredential(
		config.AzureTenantID,
		config.AzureClientID,
		config.AzureClientSecret,
		nil,
	)
	if err != nil {
		logger.Error("failed to create Azure credentials", "error", err)
		os.Exit(1)
	}

	client, err := armdns.NewRecordSetsClient(config.AzureSubID, cred, nil)
	if err != nil {
		logger.Error("failed to create Azure DNS client", "error", err)
		os.Exit(1)
	}

	updater := &DNSUpdater{
		config: config,
		logger: logger,
		client: client,
	}

	// Setup context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle OS signals for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		logger.Info("shutdown signal received")
		cancel()
	}()

	// Run the updater
	if err := updater.Run(ctx); err != nil {
		logger.Error("updater failed", "error", err)
		os.Exit(1)
	}
}

// loadConfig loads configuration from environment variables and a TOML file
func loadConfig(logger *slog.Logger) (Config, error) {
	k := koanf.New(".")

	// Load from environment variables
	if err := k.Load(env.Provider("", ".", nil), nil); err != nil {
		return Config{}, fmt.Errorf("error loading config from environment: %w", err)
	}

	// Load from TOML file if SECRETS_PATH is set
	secretsPath := k.String("SECRETS_PATH")
	if secretsPath != "" {
		if err := k.Load(file.Provider(secretsPath), toml.Parser()); err != nil {
			return Config{}, fmt.Errorf("error loading config from file: %w", err)
		}
	}

	// Parse interval
	interval, err := time.ParseDuration(k.String("INTERVAL"))
	if err != nil {
		return Config{}, fmt.Errorf("error parsing interval: %w", err)
	}

	// Parse DNS records
	dnsRecords := strings.Split(k.String("AZURE_DNS_RECORDS"), ",")
	// Filter out empty strings
	var records []string
	for _, r := range dnsRecords {
		if r = strings.TrimSpace(r); r != "" {
			records = append(records, r)
		}
	}

	return Config{
		Interval:          interval,
		AzureTenantID:     k.String("AZURE_TENANT_ID"),
		AzureClientID:     k.String("AZURE_CLIENT_ID"),
		AzureClientSecret: k.String("AZURE_CLIENT_SECRET"),
		AzureSubID:        k.String("AZURE_SUBSCRIPTION_ID"),
		AzureResourceGroup: k.String("AZURE_RESOURCE_GROUP"),
		AzureDNSZone:      k.String("AZURE_DNS_ZONE"),
		AzureDNSRecords:   records,
	}, nil
}

// Run starts the DNS updater loop
func (u *DNSUpdater) Run(ctx context.Context) error {
	ticker := time.NewTicker(u.config.Interval)
	defer ticker.Stop()

	// Do an initial update
	if err := u.checkAndUpdate(ctx); err != nil {
		u.logger.Error("initial update failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := u.checkAndUpdate(ctx); err != nil {
				u.logger.Error("update failed", "error", err)
			}
		}
	}
}

// checkAndUpdate checks the current IP and updates DNS if needed
func (u *DNSUpdater) checkAndUpdate(ctx context.Context) error {
	ip, err := u.resolvePublicIP(ctx)
	if err != nil {
		return fmt.Errorf("failed to resolve public IP: %w", err)
	}

	// If IP hasn't changed, do nothing
	if ip == u.prevIP {
		u.logger.Debug("IP unchanged", "ip", ip)
		return nil
	}

	u.logger.Info("IP changed", "ip", ip, "previous", u.prevIP)
	
	// Update all DNS records
	for _, recordSet := range u.config.AzureDNSRecords {
		if err := u.updateDNSRecord(ctx, recordSet, ip); err != nil {
			u.prevIP = "" // Reset prevIP to force retry on next run
			return fmt.Errorf("failed to update DNS record %s: %w", recordSet, err)
		}
	}

	u.prevIP = ip
	return nil
}

// resolvePublicIP gets the current public IP address
func (u *DNSUpdater) resolvePublicIP(ctx context.Context) (string, error) {
	u.logger.Debug("checking for public IP")
	
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://ifconfig.me", nil)
	if err != nil {
		return "", err
	}
	
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	
	return string(body), nil
}

// updateDNSRecord updates a single DNS record with the new IP
func (u *DNSUpdater) updateDNSRecord(ctx context.Context, recordSetName, ip string) error {
	u.logger.Info("updating DNS record", "recordset", recordSetName, "ip", ip)
	
	ttl := int64(u.config.Interval.Seconds())
	if ttl < 60 {
		ttl = 60 // Minimum TTL of 60 seconds
	}
	
	_, err := u.client.Update(
		ctx,
		u.config.AzureResourceGroup,
		u.config.AzureDNSZone,
		recordSetName,
		armdns.RecordTypeA,
		armdns.RecordSet{
			Properties: &armdns.RecordSetProperties{
				ARecords: []*armdns.ARecord{{IPv4Address: &ip}},
				TTL:      to.Ptr(ttl),
			},
		},
		&armdns.RecordSetsClientUpdateOptions{},
	)
	
	if err != nil {
		return fmt.Errorf("Azure DNS update error: %w", err)
	}
	
	u.logger.Info("DNS record updated successfully", "recordset", recordSetName)
	return nil
}
