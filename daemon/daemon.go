package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/nightlyone/lockfile"

	"github.com/manifoldco/go-base64"

	"github.com/manifoldco/torus-cli/apitypes"
	"github.com/manifoldco/torus-cli/config"
	"github.com/manifoldco/torus-cli/identity"
	"github.com/manifoldco/torus-cli/registry"

	"github.com/manifoldco/torus-cli/daemon/crypto"
	"github.com/manifoldco/torus-cli/daemon/crypto/secure"
	"github.com/manifoldco/torus-cli/daemon/db"
	"github.com/manifoldco/torus-cli/daemon/logic"
	"github.com/manifoldco/torus-cli/daemon/session"
	"github.com/manifoldco/torus-cli/daemon/socket"
	"github.com/manifoldco/torus-cli/daemon/updates"
	"github.com/manifoldco/torus-cli/daemon/utils"
)

// Daemon is the torus coprocess that contains session secrets, handles
// cryptographic operations, and communication with the registry.
type Daemon struct {
	proxy       *socket.AuthProxy
	lock        lockfile.Lockfile // actually a string
	session     session.Session
	config      *config.Config
	db          *db.DB
	logic       *logic.Engine
	updates     *updates.Engine
	hasShutdown bool
}

// New creates a new Daemon.
func New(cfg *config.Config, groupShared bool) (*Daemon, error) {
	lock, err := lockfile.New(cfg.PidPath)
	if err != nil {
		return nil, fmt.Errorf("Failed to create lockfile object: %s", err)
	}

	err = lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf(
			"Failed to create lockfile[%s]: %s", cfg.PidPath, err)
	}

	// Recover from the panic and return the error; this way we can
	// delete the lockfile!
	defer func() {
		if r := recover(); r != nil {
			err, _ = r.(error)
		}
	}()

	if groupShared {
		if err = os.Chmod(string(lock), 0640); err != nil {
			return nil, err
		}
	}

	db, err := db.NewDB(cfg.DBPath)
	if err != nil {
		return nil, err
	}

	guard := secure.NewGuard()
	session := session.NewSession(guard)
	cryptoEngine := crypto.NewEngine(session, guard)
	transport := utils.CreateHTTPTransport(cfg.CABundle, strings.Split(cfg.RegistryURI.Host, ":")[0])
	client := registry.NewClient(cfg.RegistryURI.String(), cfg.APIVersion,
		cfg.Version, session, transport)
	logic := logic.NewEngine(session, db, cryptoEngine, client, guard)

	mTransport := utils.CreateHTTPTransport(cfg.CABundle, strings.Split(cfg.ManifestURI.Host, ":")[0])
	updates := updates.NewEngine(cfg, mTransport)

	proxy, err := socket.NewAuthProxy(cfg, session, db, transport, client, logic, updates, groupShared)
	if err != nil {
		return nil, fmt.Errorf("Failed to create auth proxy: %s", err)
	}

	daemon := &Daemon{
		proxy:       proxy,
		lock:        lock,
		session:     session,
		config:      cfg,
		db:          db,
		logic:       logic,
		hasShutdown: false,
		updates:     updates,
	}

	return daemon, nil
}

// Addr returns the domain socket the Daemon is listening on.
func (d *Daemon) Addr() string {
	return d.proxy.Addr()
}

// Run starts the daemon main loop. It returns on failure, or when the daemon
// has been gracefully shut down.
func (d *Daemon) Run() error {
	email, hasEmail := os.LookupEnv("TORUS_EMAIL")
	password, hasPassword := os.LookupEnv("TORUS_PASSWORD")
	tokenID, hasTokenID := os.LookupEnv("TORUS_TOKEN_ID")
	tokenSecret, hasTokenSecret := os.LookupEnv("TORUS_TOKEN_SECRET")

	if hasEmail && hasPassword {
		log.Printf("Attempting to login as: %s", email)
		userLogin := &apitypes.UserLogin{
			Email:    email,
			Password: password,
		}

		err := d.logic.Session.Login(context.Background(), userLogin)
		if err != nil {
			return err
		}
	}

	if hasTokenID && hasTokenSecret {
		log.Printf("Attempting to login as machine token id: %s", tokenID)

		ID, err := identity.DecodeFromString(tokenID)
		if err != nil {
			log.Printf("Could not parse TORUS_TOKEN_ID")
			return err
		}

		secret, err := base64.NewFromString(tokenSecret)
		if err != nil {
			log.Printf("Could not parse TORUS_TOKEN_SECRET")
			return err
		}

		machineLogin := &apitypes.MachineLogin{
			TokenID: &ID,
			Secret:  secret,
		}

		err = d.logic.Session.Login(context.Background(), machineLogin)
		if err != nil {
			return err
		}
	}

	if err := d.updates.Start(); err != nil {
		log.Printf("cannot start updates checker: %s", err)
	}

	return d.proxy.Listen()
}

// Shutdown gracefully shuts down the daemon.
func (d *Daemon) Shutdown() error {
	if d.hasShutdown {
		return nil
	}

	d.hasShutdown = true
	if err := d.lock.Unlock(); err != nil {
		return fmt.Errorf("Could not unlock: %s", err)
	}

	if err := d.proxy.Close(); err != nil {
		return fmt.Errorf("Could not stop http proxy: %s", err)
	}

	if err := d.db.Close(); err != nil {
		return fmt.Errorf("Could not close db: %s", err)
	}

	if err := d.updates.Stop(); err != nil {
		return fmt.Errorf("Could not stop update checker: %s", err)
	}

	return nil
}
