package main

import (
	"context"
	"log"
	"net"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/dns/armdns"
	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

var k = koanf.New("")
var interval time.Duration

func main() {
	err := k.Load(env.Provider("ADU_", "", nil), nil)
	if err != nil {
		log.Fatalln("error loading config from enviroment variables:", err)
	}

	if err := k.Load(file.Provider(k.String("ADU_SECRETS_PATH")), toml.Parser()); err != nil {
		log.Fatalln("error loading config from file:", err)
	}

	interval, err = time.ParseDuration(k.String("ADU_INTERVAL"))
	if err != nil {
		log.Fatalln("error parsing interval:", err)
	}

	var prevIp string
	for {
		ip, err := resolveOwnIp()
		if err != nil {
			log.Println("error resolving own ip", err)
			continue
		}

		if ip[0] != prevIp {
			prevIp = ip[0]

			log.Printf("IP changed to %s. Updateing \n", ip[0])
			for _, set := range k.Strings("AZURE_DNS_RECORDS") {
				if !updateDns(&ip[0], set) {
					log.Println("update failed. Will retry next run")
					prevIp = ""
				}
			}
		}

		time.Sleep(interval)
	}
}

func resolveOwnIp() (addr []string, err error) {
	log.Println("checking for own ip")
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{
				Timeout: time.Millisecond * time.Duration(10000),
			}
			return d.DialContext(ctx, "udp", "resolver4.opendns.com:53")
		},
	}

	return r.LookupHost(context.Background(), "myip.opendns.com")
}

func updateDns(ip *string, recordSetName string) bool {
	log.Println("updateing recordset", recordSetName)
	cred, err := azidentity.NewClientSecretCredential(k.String("AZURE_TENANT_ID"), k.String("AZURE_CLIENT_ID"), k.String("AZURE_CLIENT_SECRET"), nil)
	if err != nil {
		log.Println("Azure crendetials error", err)
		return false
	}

	client, err := armdns.NewRecordSetsClient(k.String("AZURE_SUBSCRIPTION_ID"), cred, nil)
	if err != nil {
		log.Println("Azure DNS client error", err)
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
		log.Println("Azure DNS update error", err)
		return false
	}

	return true
}
