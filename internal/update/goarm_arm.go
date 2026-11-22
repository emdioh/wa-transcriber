//go:build arm

package update

// goarm is the GOARM level this binary was built for ("" when the build did
// not say). CheckExecutable only compares it with the asset key; the ELF
// header decides the architecture either way. The level comes from the
// goarm5/6/7 build tags, which the release build passes explicitly (the
// toolchain does not derive them from GOARM on its own).
var goarm = ""
