package model

import "regexp"

// Node IDs are stable protocol keys, not display names. Keep them within the
// database column size and usable by older agents which did not URL-escape IDs.
var nodeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func ValidNodeID(id string) bool { return nodeIDPattern.MatchString(id) }
