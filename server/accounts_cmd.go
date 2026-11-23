package archiv

import (
	"flag"
	"fmt"
	"log"
	"os"
)

// userCmd manages the accounts of an archive with auth = local from the
// command line:
//
//	archiv user add -login NAME [-name "Voller Name"] [-email ADDR] [-role archivar|submitter|reader]
//	archiv user link -login NAME     a new link to set the password
//	archiv user list
func userCmd(args []string) {
	if len(args) == 0 {
		log.Fatal("usage: archiv user add|link|list [flags]")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("user "+sub, flag.ExitOnError)
	login := fs.String("login", "", "login name")
	name := fs.String("name", "", "display name")
	email := fs.String("email", "", "email address")
	role := fs.String("role", RoleSubmitter, "reader, submitter or archivar")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		log.Fatal(err)
	}
	db, err := openDB(cfg.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	a := &App{cfg: cfg, db: db, names: newNames(db)}
	a.people = newPeople(db)
	if a.accounts, err = newAccounts(a); err != nil {
		log.Fatal(err)
	}
	ac := a.accounts
	switch sub {
	case "add":
		acc, err := ac.create(*login, *name, *email, *role)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Konto %s (%s) angelegt. Passwort setzen: %s\n", acc.Login, acc.RoleLabel(), ac.passwordLink(acc.ID))
	case "link":
		acc, err := ac.byLogin(*login)
		if err != nil {
			log.Fatalf("Kein Konto %q", *login)
		}
		fmt.Printf("Passwort setzen für %s: %s\n", acc.Login, ac.passwordLink(acc.ID))
	case "list":
		for _, acc := range ac.list() {
			state := ""
			if acc.Disabled {
				state = " (gesperrt)"
			}
			fmt.Fprintf(os.Stdout, "%-20s %-10s %s%s\n", acc.Login, acc.RoleLabel(), acc.Name, state)
		}
	default:
		log.Fatalf("unknown: user %s", sub)
	}
}
