package engine

import (
	"errors"
	"net/url"
	"strings"
)

// ErrorText is err's message for the shipped logs. The one place a secret
// enters an error message is a *url.Error, which names the request URL, and
// RPC URLs carry their API key; each is replaced by its operation and cause.
// Database errors name host, user and database, never the password, and
// this module never puts key material in an error.
func ErrorText(err error) string {
	message := err.Error()
	var request *url.Error
	for errors.As(err, &request) {
		message = strings.ReplaceAll(message, request.Error(), request.Op+": "+request.Err.Error())
		err = request.Err
	}
	return message
}
