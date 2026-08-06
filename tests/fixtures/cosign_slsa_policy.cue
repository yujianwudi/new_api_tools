package newapi_tools_release

"_type": "https://in-toto.io/Statement/v0.1"
predicateType: "https://slsa.dev/provenance/v1"
subject: [{
	name: "ghcr.io/yujianwudi/new_api_tools"
	digest: sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}]
predicate: {
	buildDefinition: {
		buildType: "https://github.com/yujianwudi/new_api_tools/.github/workflows/release-recovery.yml@refs/heads/main"
		externalParameters: {
			repository:      "https://github.com/yujianwudi/new_api_tools"
			ref:             "refs/tags/v0.6.2"
			revision:        "dddddddddddddddddddddddddddddddddddddddd"
			tag:             "v0.6.2"
			tag_oid:         "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
			manifest_digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			platform_digests: close({
				"linux/amd64": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				"linux/arm64": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
			})
		}
		internalParameters: {
			workflow_ref: "yujianwudi/new_api_tools/.github/workflows/release-recovery.yml@refs/heads/main"
			workflow_sha: "ffffffffffffffffffffffffffffffffffffffff"
		}
		resolvedDependencies: [{
			uri: "git+https://github.com/yujianwudi/new_api_tools@refs/tags/v0.6.2"
			digest: gitCommit: "dddddddddddddddddddddddddddddddddddddddd"
		}]
	}
	runDetails: builder: id: "https://github.com/yujianwudi/new_api_tools/.github/workflows/release-recovery.yml@refs/heads/main"
}
