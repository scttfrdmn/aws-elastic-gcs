// Package gcsid validates Globus Connect Server object IDs before they are
// interpolated into shell commands run as root via SSM RunCommand.
//
// GCS node, collection, and storage gateway IDs are UUIDs. Values reach this
// code from EC2 tags (which instances can write), GCS CLI JSON output, and
// operator input, so treat every ID as untrusted until it passes Validate.
package gcsid

import (
	"fmt"
	"regexp"
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Validate returns an error unless id is a canonical UUID.
func Validate(id string) error {
	if !uuidRe.MatchString(id) {
		return fmt.Errorf("invalid GCS ID %q: expected a UUID", id)
	}
	return nil
}
