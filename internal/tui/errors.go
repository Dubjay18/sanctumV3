package tui

import (
	"strings"
)

// userError maps technical internal error structures to clean, user-friendly error messages.
func userError(err error) string {
	if err == nil {
		return ""
	}
	return userErrorStr(err.Error())
}

// userErrorStr maps technical internal error strings to clean, user-friendly error messages.
func userErrorStr(msg string) string {
	switch {
	case strings.Contains(msg, "not_authorized") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "permission denied") || strings.Contains(msg, "PermissionDenied"):
		return "Access denied. You do not have permission to perform this action."
	case strings.Contains(msg, "not found") || strings.Contains(msg, "NotFound") || strings.Contains(msg, "codes.NotFound") || strings.Contains(msg, "room_not_found"):
		return "The requested room or message could not be found."
	case strings.Contains(msg, "unavailable") || strings.Contains(msg, "Unavailable") || strings.Contains(msg, "offline") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "database_offline"):
		return "Sanctum server is temporarily offline or database is unavailable. Retrying..."
	case strings.Contains(msg, "invalid token") || strings.Contains(msg, "token expired") || strings.Contains(msg, "securetoken.googleapis.com"):
		return "Your session has expired. Please log in again."
	default:
		return "Something went wrong. Check logs."
	}
}
