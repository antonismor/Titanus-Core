package main

import (
	"flag"
	"fmt"
	"math/big"
	"time"

	"github.com/antonismor/Titanus-Core/internal/identity"
)

func runIdentity(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: titanus identity <issue|renew|revoke|refresh-crl> [flags]")
	}
	fs := flag.NewFlagSet("identity "+args[0], flag.ContinueOnError)
	dir := fs.String("pki-dir", "/etc/titanus/pki", "Realm CA directory")
	realm := fs.String("realm", "TITANUS-REALM", "Realm name")
	id := fs.String("id", "", "identity ID")
	role := fs.String("role", "node", "node, controller or admin")
	address := fs.String("address", "", "server address SAN")
	ttl := fs.Duration("ttl", 24*time.Hour, "certificate lifetime (1h to 30d)")
	serialText := fs.String("serial", "", "certificate serial in hexadecimal")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	auth, err := identity.InitAuthority(*dir, *realm)
	if err != nil {
		return err
	}
	switch args[0] {
	case "issue", "renew":
		cert, key, err := auth.Issue(*id, []string{*address}, identity.Role(*role), *ttl)
		if err != nil {
			return err
		}
		fmt.Printf("CA: %s\nCert: %s\nKey: %s\n", auth.CertPath, cert, key)
	case "revoke", "refresh-crl":
		var serial *big.Int
		if args[0] == "revoke" {
			var ok bool
			serial, ok = new(big.Int).SetString(*serialText, 16)
			if !ok {
				return fmt.Errorf("--serial must be hexadecimal")
			}
		}
		path, err := auth.Revoke(serial)
		if err != nil {
			return err
		}
		fmt.Println(path)
	default:
		return fmt.Errorf("unknown identity action %q", args[0])
	}
	return nil
}
