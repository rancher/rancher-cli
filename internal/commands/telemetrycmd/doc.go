// Package telemetrycmd holds the local commands of the telemetry group:
// status, on, off, reset, show and upload. They read and change the recording
// setting of one config directory and the files recorded below it. They read
// no credential; upload sends to the experiment endpoint through a function
// internal/cli passes it, and the others make no request to any server.
package telemetrycmd
