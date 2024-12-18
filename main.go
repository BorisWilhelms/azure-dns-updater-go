package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"
	"io/ioutil"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/dns/armdns"
	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"golang.org/x/exp/slog"
)

var k = koanf.New("")
var interval time.Duration
var logger *slog.Logger

func main() {
	logger = slog.Default()
	err := k.Load(env.Provider("", "", nil), nil)
	if err != nil {
		logger.Error("error loading config from environment variables:", err)
		os.Exit(1)
	}

	if err := k.Load(file.Provider(k.String("SECRETS_PATH")), toml.Parser()); err != nil {
		logger.Error("error loading config from file:", err)
		os.Exit(1)
	}

	interval, err = time.ParseDuration(k.String("INTERVAL"))
	if err != nil {
		logger.Error("error parsing interval:", err)
		os.Exit(1)
	}
	var records = strings.Split(k.String("AZURE_DNS_RECORDS"), ",")
	var prevIp string
	for {
		ip, err := resolveOwnIp()
		if err != nil {
			logger.Error("error resolving own ip", err)
			continue
		}

		if ip != prevIp {
			prevIp = ip

			logger.Info("IP changed", slog.String("ip", ip))
			for _, set := range records {
				if !updateDns(&ip, set) {
					logger.Error("update failed. Will retry next run")
					prevIp = ""
				}
			}
		}

		time.Sleep(interval)
	}
}

func resolveOwnIp() (addr string, err error) {
	logger.Debug("checking for own ip")
	req, err := http.Get("https://ifconfig.me")
	if err != nil {
		return "", err
	}
	defer req.Body.Close()
	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func updateDns(ip *string, recordSetName string) bool {
	logger.Info("updateing recordset", slog.String("recordset", recordSetName))
	cred, err := azidentity.NewClientSecretCredential(k.String("AZURE_TENANT_ID"), k.String("AZURE_CLIENT_ID"), k.String("AZURE_CLIENT_SECRET"), nil)
	if err != nil {
		logger.Error("Azure crendetials error", err)
		return false
	}

	client, err := armdns.NewRecordSetsClient(k.String("AZURE_SUBSCRIPTION_ID"), cred, nil)
	if err != nil {
		logger.Error("Azure DNS client error", err)
		return false
	}

	_, err = client.Update(context.Background(),
		k.String("AZURE_RESOURCE_GROUP"),
		k.String("AZURE_DNS_ZONE"),
		recordSetName,
		armdns.RecordTypeA,
		armdns.RecordSet{
			Properties: &armdns.RecordSetProperties{
				ARecords: []*armdns.ARecord{{IPv4Address: ip}},
				TTL:      to.Ptr(int64(interval.Seconds())),
			},
		},
		&armdns.RecordSetsClientUpdateOptions{IfMatch: nil})

	if err != nil {
		logger.Error("Azure DNS update error", err)
		return false
	}

	return true
}
