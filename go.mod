module github.com/TiaraBasori/PaperValet-Plugins

go 1.25.0

require github.com/TiaraBasori/PaperValet v0.1.0

// In CI, this replace is handled by the workflow
// For local development, uncomment or set with:
//   go mod edit -replace github.com/TiaraBasori/PaperValet=/path/to/PaperValet