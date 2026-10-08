module github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-build

go 1.25.0

replace github.com/puppet-stagehand/stagehand-sdk => ../..

require (
	github.com/evanw/esbuild v0.28.2
	github.com/puppet-stagehand/stagehand-sdk v0.0.0-00010101000000-000000000000
)

require golang.org/x/sys v0.47.0 // indirect
