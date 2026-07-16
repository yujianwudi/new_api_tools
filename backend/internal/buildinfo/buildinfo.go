package buildinfo

// These values can be replaced at build time with -ldflags. Keeping a real
// semantic version as the default makes local and source builds identifiable.
var (
	Version   = "0.5.1"
	Commit    = "unknown"
	BuildDate = "unknown"
)
