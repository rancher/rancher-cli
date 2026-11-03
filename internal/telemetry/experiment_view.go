package telemetry

// ExperimentView is what `telemetry status` shows of the experiments turned
// on for this run. The zero value shows nothing.
type ExperimentView struct {
	// Names are the active experiments, in the order the variable lists them.
	Names []string
	// Endpoint is the canonical upload origin, or "" when none is set.
	Endpoint string
	// EndpointProblem is why the telemetryupload value was refused; "" when it
	// was accepted, was not set or has no value. It never holds the value.
	EndpointProblem string
	// Warnings are the lines about items of the variable that were ignored.
	Warnings []string
}

// Active reports whether an experiment is on.
func (v ExperimentView) Active() bool { return len(v.Names) > 0 }
