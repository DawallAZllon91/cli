package migration

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/cli/cli/v2/internal/keyring"
	ghAPI "github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/config"
)

type CowardlyRefusalError struct {
	Reason string
}

func (e CowardlyRefusalError) Error() string {
	// Consider whether we should add a call to action here like "open an issue with the contents of your redacted hosts.yml"
	return fmt.Sprintf("cowardly refusing to continue with multi account migration: %s", e.Reason)
}

var hostsKey = []string{"hosts"}

// This migration exists to take a hosts section of the following structure:
//
//	github.com:
//	  user: williammartin
//	  git_protocol: https
//	  editor: vim
//	github.localhost:
//	  user: monalisa
//	  git_protocol: https
//    oauth_token: xyz
//
// We want this to migrate to something like:
//
// github.com:
//	 user: williammartin
//	 git_protocol: https
//	 editor: vim
//	 users:
//	   williammartin:
//	     git_protocol: https
//	     editor: vim
//
// github.localhost:
//	 user: monalisa
//	 git_protocol: https
//   oauth_token: xyz
//   users:
//	   monalisa:
//	     git_protocol: https
//	     oauth_token: xyz
//
// The reason for this is that we can then add new users under a host.
// Note that we are only copying the config under a new users key, and
// under a specific user. The original config is left alone. This is to
// allow forward compatability for older versions of gh and also to avoid
// breaking existing users of go-gh which looks at a specific location
// in the config for oauth tokens that are stored insecurely.

type MultiAccount struct {
	// Allow injecting a transport layer in tests.
	Transport http.RoundTripper
}

func (m MultiAccount) PreVersion() string {
	return ""
}

func (m MultiAccount) PostVersion() string {
	return "1"
}

func (m MultiAccount) Do(c *config.Config) error {
	hostnames, err := c.Keys(hostsKey)
	// [github.com, github.localhost]
	// We wouldn't expect to have a hosts key when this is the first time anyone
	// is logging in with the CLI.
	var keyNotFoundError *config.KeyNotFoundError
	if errors.As(err, &keyNotFoundError) {
		return nil
	}
	if err != nil {
		return CowardlyRefusalError{"couldn't get hosts configuration"}
	}

	// If there are no hosts then it doesn't matter whether we migrate or not,
	// so lets avoid any confusion and say there's no migration required.
	if len(hostnames) == 0 {
		return nil
	}

	// Otherwise let's get to the business of migrating!
	for _, hostname := range hostnames {
		configEntryKeys, err := c.Keys(append(hostsKey, hostname))
		// e.g. [user, git_protocol, editor, ouath_token]
		if err != nil {
			return CowardlyRefusalError{fmt.Sprintf("couldn't get host configuration despite %q existing", hostname)}
		}

		// Get the user so that we can nest under it in future
		username, err := c.Get(append(hostsKey, hostname, "user"))
		if err != nil {
			return CowardlyRefusalError{fmt.Sprintf("couldn't get user name for %q", hostname)}
		}

		// When anonymous user exists get the user login.
		if username == "x-access-token" {
			var token string
			token, err := c.Get(append(hostsKey, hostname, "oauth_token"))
			if err != nil || token == "" {
				token, err = keyring.Get(keyringServiceName(hostname), "")
			}
			if err != nil || token == "" {
				return CowardlyRefusalError{fmt.Sprintf("couldn't find oauth token for %q", hostname)}
			}
			username, err = getUsername(m.Transport, hostname, token)
			if err != nil {
				return CowardlyRefusalError{fmt.Sprintf("couldn't retrieve logged in user for %q", hostname)}
			}
			c.Set(append(hostsKey, hostname, "user"), username)
		}

		// Create the username key with an empty value so it will be
		// written even if there are no keys set under it.
		c.Set(append(hostsKey, hostname, "users", username), "")

		for _, configEntryKey := range configEntryKeys {
			// Do not re-write the user key.
			if configEntryKey == "user" {
				continue
			}

			// We would expect that these keys map directly to values
			// e.g. [williammartin, https, vim, gho_xyz...] but it's possible that a manually
			// edited config file might nest further but we don't support that.
			//
			// We could consider throwing away the nested values, but I suppose
			// I'd rather make the user take a destructive action even if we have a backup.
			// If they have configuration here, it's probably for a reason.
			keys, err := c.Keys(append(hostsKey, hostname, configEntryKey))
			if err == nil && len(keys) > 0 {
				return CowardlyRefusalError{"hosts file has entries that are surprisingly deeply nested"}
			}

			configEntryValue, err := c.Get(append(hostsKey, hostname, configEntryKey))
			if err != nil {
				return CowardlyRefusalError{fmt.Sprintf("couldn't get configuration entry value despite %q / %q existing", hostname, configEntryKey)}
			}

			// Set these entries in their new location under the user
			c.Set(append(hostsKey, hostname, "users", username, configEntryKey), configEntryValue)
		}
	}

	return nil
}

func getUsername(transport http.RoundTripper, hostname, token string) (string, error) {
	opts := ghAPI.ClientOptions{
		Host:      hostname,
		AuthToken: token,
		Transport: transport,
	}
	client, err := ghAPI.NewGraphQLClient(opts)
	if err != nil {
		return "", err
	}
	var query struct {
		Viewer struct {
			Login string
		}
	}
	err = client.Query("CurrentUser", &query, nil)
	return query.Viewer.Login, err
}

func keyringServiceName(hostname string) string {
	return "gh:" + hostname
}
