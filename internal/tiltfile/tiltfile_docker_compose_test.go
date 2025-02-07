//+build !skiplargetiltfiletests

package tiltfile

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tilt-dev/tilt/pkg/model"
)

const simpleConfig = `version: '3'
services:
  foo:
    build: ./foo
    command: sleep 100
    ports:
      - "12312:80"`

const configWithMounts = `version: '3.2'
services:
  foo:
    build: ./foo
    command: sleep 100
    volumes:
      - ./foo:/foo
      # these volumes are currently unsupported, but included here to ensure we don't blow up on them
      - bar:/bar
      - type: volume
        source: baz
        target: /baz
    ports:
      - "12312:80"
volumes:
  bar: {}
  baz: {}`

const barServiceConfig = `version: '3'
services:
  bar:
    image: bar-image
    expose:
      - "3000"
    depends_on:
      - foo
`

const twoServiceConfig = `version: '3'
services:
  foo:
    build: ./foo
    command: sleep 100
    ports:
      - "12312:80"
  bar:
    image: bar-image
    expose:
      - "3000"
    depends_on:
      - foo
`

// YAML for Foo config looks a little different from the above after being read into
// a struct and YAML'd back out...
func (f *fixture) simpleConfigAfterParse() string {
	return fmt.Sprintf(`build:
  context: %s
command: sleep 100
ports:
- published: 12312
  target: 80`, f.JoinPath("foo"))
}

func TestDockerComposeManifest(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.file("docker-compose.yml", simpleConfig)
	f.file("Tiltfile", "docker_compose('docker-compose.yml')")

	f.load("foo")
	configPath := f.TempDirFixture.JoinPath("docker-compose.yml")
	f.assertDcManifest("foo",
		dcConfigPath([]string{configPath}),
		dcYAMLRaw(f.simpleConfigAfterParse()),
		dcDfRaw(simpleDockerfile),
		dcPublishedPorts(12312),
		// TODO(maia): assert m.tiltFilename
	)

	expectedConfFiles := []string{
		"Tiltfile",
		".tiltignore",
		".dockerignore",
		"docker-compose.yml",
		filepath.Join("foo", "Dockerfile"),
		filepath.Join("foo", ".dockerignore"),
	}
	f.assertConfigFiles(expectedConfFiles...)
}

func TestDockerComposeManifestNoDockerfile(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.file("docker-compose.yml", `version: '3'
services:
  bar:
    image: redis:alpine`)
	f.file("Tiltfile", "docker_compose('docker-compose.yml')")

	f.load("bar")
	configPath := f.TempDirFixture.JoinPath("docker-compose.yml")
	f.assertDcManifest("bar",
		dcConfigPath([]string{configPath}),
		dcYAMLRaw("image: redis:alpine"),
		dcDfRaw(""),
		// TODO(maia): assert m.tiltFilename
	)

	expectedConfFiles := []string{"Tiltfile", ".tiltignore", "docker-compose.yml"}
	f.assertConfigFiles(expectedConfFiles...)
}

func TestDockerComposeManifestAlternateDockerfile(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	dcYAML := fmt.Sprintf(`build:
  context: %s
  dockerfile: alternate-Dockerfile`,
		f.JoinPath("baz"))
	f.dockerfile("baz/alternate-Dockerfile")
	f.file("docker-compose.yml", fmt.Sprintf(`
version: '3'
services:
  baz:
    build:
      context: %s
      dockerfile: alternate-Dockerfile`, f.JoinPath("baz")))
	f.file("Tiltfile", "docker_compose('docker-compose.yml')")

	f.load("baz")
	configPath := f.TempDirFixture.JoinPath("docker-compose.yml")
	f.assertDcManifest("baz",
		dcConfigPath([]string{configPath}),
		dcYAMLRaw(dcYAML),
		dcDfRaw(simpleDockerfile),
		// TODO(maia): assert m.tiltFilename
	)

	expectedConfFiles := []string{"Tiltfile", ".tiltignore", ".dockerignore", "docker-compose.yml", "baz/alternate-Dockerfile", "baz/.dockerignore"}
	f.assertConfigFiles(expectedConfFiles...)
}

func TestDockerComposeManifestAlternateDockerfileAndDockerIgnore(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	dcYAML := fmt.Sprintf(`build:
  context: %s
  dockerfile: alternate-Dockerfile`,
		f.JoinPath("baz"))
	f.dockerfile("baz/alternate-Dockerfile")
	f.dockerignore("baz/alternate-Dockerfile.dockerignore")
	f.file("docker-compose.yml", fmt.Sprintf(`
version: '3'
services:
  baz:
    build:
      context: %s
      dockerfile: alternate-Dockerfile`, f.JoinPath("baz")))
	f.file("Tiltfile", "docker_compose('docker-compose.yml')")

	f.load("baz")
	configPath := f.TempDirFixture.JoinPath("docker-compose.yml")
	f.assertDcManifest("baz",
		dcConfigPath([]string{configPath}),
		dcYAMLRaw(dcYAML),
		dcDfRaw(simpleDockerfile),
		// TODO(maia): assert m.tiltFilename
	)

	expectedConfFiles := []string{"Tiltfile", ".tiltignore", "docker-compose.yml", "baz/alternate-Dockerfile", "baz/alternate-Dockerfile.dockerignore"}
	f.assertConfigFiles(expectedConfFiles...)
}

func TestMultipleDockerComposeDifferentDirsNotSupported(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.file("docker-compose1.yml", simpleConfig)

	f.dockerfile(filepath.Join("subdir", "foo", "Dockerfile"))
	f.file(filepath.Join("subdir", "Tiltfile"), `docker_compose('docker-compose2.yml')`)
	f.file(filepath.Join("subdir", "docker-compose2.yml"), simpleConfig)

	tf := `
include('./subdir/Tiltfile')
docker_compose('docker-compose1.yml')`
	f.file("Tiltfile", tf)

	f.loadErrString("Cannot load docker-compose files from two different Tiltfiles")
}

func TestMultipleDockerComposeSameDir(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.file("docker-compose1.yml", simpleConfig)
	f.file("docker-compose2.yml", barServiceConfig)

	tf := `
docker_compose('docker-compose1.yml')
docker_compose('docker-compose2.yml')`
	f.file("Tiltfile", tf)

	f.load()

	assert.Equal(t, 2, len(f.loadResult.Manifests))
}

func TestDockerComposeAndK8sNotSupported(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.setupFoo()
	f.file("docker-compose.yml", simpleConfig)
	tf := `docker_compose('docker-compose.yml')
docker_build('gcr.io/foo', 'foo')
k8s_yaml('foo.yaml')`
	f.file("Tiltfile", tf)

	f.loadErrString("can't declare both k8s " +
		"resources/entities and docker-compose resources")
}

func TestDockerComposeResourceCreationFromAbsPath(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	configPath := f.TempDirFixture.JoinPath("docker-compose.yml")
	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.file("docker-compose.yml", `
version: '3'
services:
  foo:
    build: ./foo
    command: sleep 100
    ports:
      - "12312:80"`)
	f.file("Tiltfile", fmt.Sprintf("docker_compose(%q)", configPath))

	f.load("foo")
	f.assertDcManifest("foo", dcConfigPath([]string{configPath}))
}

func TestDockerComposeManifestComputesLocalPaths(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	df := `FROM alpine

ADD ./src /app
COPY ./thing.go /stuff
RUN echo hi`
	f.file(filepath.Join("foo", "Dockerfile"), df)

	f.file("docker-compose.yml", simpleConfig)
	f.file("Tiltfile", "docker_compose('docker-compose.yml')")

	f.load("foo")
	configPath := f.JoinPath("docker-compose.yml")
	f.assertDcManifest("foo",
		dcConfigPath([]string{configPath}),
		dcYAMLRaw(f.simpleConfigAfterParse()),
		dcDfRaw(df),
		dcLocalPaths([]string{f.JoinPath("foo")}),
		// TODO(maia): assert m.tiltFilename
	)

	expectedConfFiles := []string{
		"Tiltfile",
		".tiltignore",
		"docker-compose.yml",
		filepath.Join("foo", "Dockerfile"),
		".dockerignore",
		filepath.Join("foo", ".dockerignore"),
	}
	f.assertConfigFiles(expectedConfFiles...)
}

func TestDockerComposeMultiStageBuild(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	df := `FROM alpine as builder
ADD ./src /app
RUN echo hi

FROM alpine
COPY --from=builder /app /app
RUN echo bye`
	f.file(filepath.Join("foo", "Dockerfile"), df)
	f.file(filepath.Join("foo", "docker-compose.yml"), `version: '3'
services:
  foo:
    build:
      context: ./
    command: sleep 100
    ports:
      - "12312:80"`)
	f.file("Tiltfile", "docker_compose('foo/docker-compose.yml')")
	f.load("foo")
	configPath := f.JoinPath("foo", "docker-compose.yml")
	f.assertDcManifest("foo",
		dcConfigPath([]string{configPath}),
		dcYAMLRaw(f.simpleConfigAfterParse()),
		dcDfRaw(df),
		dcLocalPaths([]string{f.JoinPath("foo")}),
		dcPublishedPorts(12312),
	)

	expectedConfFiles := []string{
		"Tiltfile",
		".tiltignore",
		filepath.Join("foo", "docker-compose.yml"),
		filepath.Join("foo", "Dockerfile"),
		".dockerignore",
		filepath.Join("foo", ".dockerignore"),
	}
	f.assertConfigFiles(expectedConfFiles...)
}

func TestDockerComposeHonorsDockerIgnore(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	df := `FROM alpine

ADD . /app
COPY ./thing.go /stuff
RUN echo hi`
	f.file(filepath.Join("foo", "Dockerfile"), df)

	f.file("docker-compose.yml", simpleConfig)
	f.file("Tiltfile", "docker_compose('docker-compose.yml')")

	f.file(filepath.Join("foo", ".dockerignore"), "tmp")
	f.file(".dockerignore", "foo/tmp2")

	f.load("foo")

	f.assertNextManifest("foo",
		buildFilters(filepath.Join("foo", "tmp2")),
		fileChangeFilters(filepath.Join("foo", "tmp2")),
		buildFilters(filepath.Join("foo", "tmp")),
		fileChangeFilters(filepath.Join("foo", "tmp")),
	)
}

func TestDockerComposeIgnoresFileChangesOnMountedVolumes(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	df := `FROM alpine

ADD . /app
COPY ./thing.go /stuff
RUN echo hi`
	f.file(filepath.Join("foo", "Dockerfile"), df)

	f.file("docker-compose.yml", configWithMounts)
	f.file("Tiltfile", "docker_compose('docker-compose.yml')")

	f.load("foo")

	f.assertNextManifest("foo",
		// ensure that DC syncs are *not* ignored for builds, because all files are still relevant to builds
		buildMatches(filepath.Join("foo", "Dockerfile")),
		// ensure that DC syncs *are* ignored for file watching, i.e., won't trigger builds
		fileChangeFilters(filepath.Join("foo", "blah")),
	)
}

func TestDockerComposeWithDockerBuild(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.file("docker-compose.yml", simpleConfig)
	f.file("Tiltfile", `docker_build('gcr.io/foo', './foo')
docker_compose('docker-compose.yml')
dc_resource('foo', 'gcr.io/foo')
`)

	f.load()

	m := f.assertNextManifest("foo", db(image("gcr.io/foo")))
	iTarget := m.ImageTargetAt(0)

	// Make sure there's no live update in the default case.
	assert.True(t, iTarget.IsDockerBuild())
	assert.True(t, iTarget.LiveUpdateInfo().Empty())

	configPath := f.TempDirFixture.JoinPath("docker-compose.yml")
	assert.Equal(t, m.DockerComposeTarget().ConfigPaths, []string{configPath})
}

func TestDockerComposeWithDockerBuildAutoAssociate(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.file("docker-compose.yml", `version: '3'
services:
  foo:
    image: gcr.io/as_specified_in_config
    build: ./foo
    command: sleep 100
    ports:
      - "12312:80"`)
	f.file("Tiltfile", `docker_build('gcr.io/as_specified_in_config', './foo')
docker_compose('docker-compose.yml')
`)

	f.load()

	// don't need a dc_resource call if the docker_build image matches the
	// `Image` specified in dc.yml
	m := f.assertNextManifest("foo", db(image("gcr.io/as_specified_in_config")))
	iTarget := m.ImageTargetAt(0)

	// Make sure there's no live update in the default case.
	assert.True(t, iTarget.IsDockerBuild())
	assert.True(t, iTarget.LiveUpdateInfo().Empty())

	configPath := f.TempDirFixture.JoinPath("docker-compose.yml")
	assert.Equal(t, m.DockerComposeTarget().ConfigPaths, []string{configPath})
}

// I.e. make sure that we handle de/normalization between `fooimage` <--> `docker.io/library/fooimage`
func TestDockerComposeWithDockerBuildLocalRef(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.file("docker-compose.yml", simpleConfig)
	f.file("Tiltfile", `docker_build('fooimage', './foo')
docker_compose('docker-compose.yml')
dc_resource('foo', 'fooimage')
`)

	f.load()

	m := f.assertNextManifest("foo", db(image("fooimage")))
	assert.True(t, m.ImageTargetAt(0).IsDockerBuild())

	configPath := f.TempDirFixture.JoinPath("docker-compose.yml")
	assert.Equal(t, m.DockerComposeTarget().ConfigPaths, []string{configPath})
}

func TestMultipleDockerComposeWithDockerBuild(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.dockerfile(filepath.Join("bar", "Dockerfile"))
	f.file("docker-compose.yml", twoServiceConfig)
	f.file("Tiltfile", `docker_build('gcr.io/foo', './foo')
docker_build('gcr.io/bar', './bar')
docker_compose('docker-compose.yml')
dc_resource('foo', 'gcr.io/foo')
dc_resource('bar', 'gcr.io/bar')
`)

	f.load()

	foo := f.assertNextManifest("foo", db(image("gcr.io/foo")))
	assert.True(t, foo.ImageTargetAt(0).IsDockerBuild())

	bar := f.assertNextManifest("bar", db(image("gcr.io/bar")))
	assert.True(t, foo.ImageTargetAt(0).IsDockerBuild())

	configPath := f.TempDirFixture.JoinPath("docker-compose.yml")
	assert.Equal(t, foo.DockerComposeTarget().ConfigPaths, []string{configPath})
	assert.Equal(t, bar.DockerComposeTarget().ConfigPaths, []string{configPath})
}

func TestMultipleDockerComposeWithDockerBuildImageNames(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.dockerfile(filepath.Join("bar", "Dockerfile"))
	f.file("docker-compose.yml", `version: '3'
services:
  foo:
    image: gcr.io/foo
  bar:
    image: gcr.io/bar
`)
	f.file("Tiltfile", `
docker_build('gcr.io/foo', './foo')
docker_build('gcr.io/bar', './bar')
docker_compose('docker-compose.yml')
`)

	f.load()

	foo := f.assertNextManifest("foo", db(image("gcr.io/foo")))
	assert.True(t, foo.ImageTargetAt(0).IsDockerBuild())

	bar := f.assertNextManifest("bar", db(image("gcr.io/bar")))
	assert.True(t, foo.ImageTargetAt(0).IsDockerBuild())

	configPath := f.TempDirFixture.JoinPath("docker-compose.yml")
	assert.Equal(t, foo.DockerComposeTarget().ConfigPaths, []string{configPath})
	assert.Equal(t, bar.DockerComposeTarget().ConfigPaths, []string{configPath})
}

func TestDCImageRefSuggestion(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.setupFoo()
	f.file("docker-compose.yml", `version: '3'
services:
  foo:
    image: gcr.io/foo
`)
	f.file("Tiltfile", `
docker_build('gcr.typo.io/foo', 'foo')
docker_compose('docker-compose.yml')
`)
	f.loadAssertWarnings(`Image not used in any Docker Compose config:
    ✕ gcr.typo.io/foo
Did you mean…
    - gcr.io/foo
Skipping this image build`)
}

func TestDockerComposeOnlySomeWithDockerBuild(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.file("docker-compose.yml", twoServiceConfig)
	f.file("Tiltfile", `img_name = 'gcr.io/foo'
docker_build(img_name, './foo')
docker_compose('docker-compose.yml')
dc_resource('foo', img_name)
`)

	f.load()

	foo := f.assertNextManifest("foo", db(image("gcr.io/foo")))
	assert.True(t, foo.ImageTargetAt(0).IsDockerBuild())

	bar := f.assertNextManifest("bar")
	assert.Empty(t, bar.ImageTargets)

	configPath := f.TempDirFixture.JoinPath("docker-compose.yml")
	assert.Equal(t, foo.DockerComposeTarget().ConfigPaths, []string{configPath})
	assert.Equal(t, bar.DockerComposeTarget().ConfigPaths, []string{configPath})
}

func TestDockerComposeResourceNoImageMatch(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.file("docker-compose.yml", simpleConfig)
	f.file("Tiltfile", `docker_build('gcr.io/foo', './foo')
docker_compose('docker-compose.yml')
dc_resource('no-svc-with-this-name-eek', 'gcr.io/foo')
`)
	f.loadErrString("no Docker Compose service found with name")
}

func TestDockerComposeLoadConfigFilesOnFailure(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.file("docker-compose.yml", simpleConfig)
	f.file("Tiltfile", `docker_build('gcr.io/foo', './foo')
docker_compose('docker-compose.yml')
fail("deliberate exit")
`)
	f.loadErrString("deliberate exit")

	// Make sure that even though tiltfile execution failed, we still
	// loaded config files correctly.
	f.assertConfigFiles(".tiltignore", "Tiltfile", "docker-compose.yml", "foo/Dockerfile")
}

func TestDockerComposeDoesntSupportEntrypointOverride(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.file("docker-compose.yml", simpleConfig)
	f.file("Tiltfile", `docker_build('gcr.io/foo', './foo', entrypoint='./foo')
docker_compose('docker-compose.yml')
dc_resource('foo', 'gcr.io/foo')
`)

	f.loadErrString("docker_build/custom_build.entrypoint not supported for Docker Compose resources")
}

func TestDefaultRegistryWithDockerCompose(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.file("docker-compose.yml", simpleConfig)
	f.file("Tiltfile", `
docker_compose('docker-compose.yml')
default_registry('bar.com')
`)

	f.loadErrString("default_registry is not supported with docker compose")
}

func TestTriggerModeDC(t *testing.T) {
	for _, testCase := range []struct {
		name                string
		globalSetting       triggerMode
		dcResourceSetting   triggerMode
		expectedTriggerMode model.TriggerMode
	}{
		{"default", TriggerModeUnset, TriggerModeUnset, model.TriggerModeAuto},
		{"explicit global auto", TriggerModeAuto, TriggerModeUnset, model.TriggerModeAuto},
		{"explicit global manual", TriggerModeManual, TriggerModeUnset, model.TriggerModeManualWithAutoInit},
		{"dc auto", TriggerModeUnset, TriggerModeUnset, model.TriggerModeAuto},
		{"dc manual", TriggerModeUnset, TriggerModeManual, model.TriggerModeManualWithAutoInit},
		{"dc override auto", TriggerModeManual, TriggerModeAuto, model.TriggerModeAuto},
		{"dc override manual", TriggerModeAuto, TriggerModeManual, model.TriggerModeManualWithAutoInit},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture(t)
			defer f.TearDown()

			f.dockerfile(filepath.Join("foo", "Dockerfile"))
			f.file("docker-compose.yml", simpleConfig)

			var globalTriggerModeDirective string
			switch testCase.globalSetting {
			case TriggerModeUnset:
				globalTriggerModeDirective = ""
			case TriggerModeManual:
				globalTriggerModeDirective = "trigger_mode(TRIGGER_MODE_MANUAL)"
			case TriggerModeAuto:
				globalTriggerModeDirective = "trigger_mode(TRIGGER_MODE_AUTO)"
			}

			var dcResourceDirective string
			switch testCase.dcResourceSetting {
			case TriggerModeUnset:
				dcResourceDirective = ""
			case TriggerModeManual:
				dcResourceDirective = "dc_resource('foo', trigger_mode=TRIGGER_MODE_MANUAL)"
			case TriggerModeAuto:
				dcResourceDirective = "dc_resource('foo', trigger_mode=TRIGGER_MODE_AUTO)"
			}

			f.file("Tiltfile", fmt.Sprintf(`
%s
docker_compose('docker-compose.yml')
%s
`, globalTriggerModeDirective, dcResourceDirective))

			f.load()

			f.assertNumManifests(1)
			f.assertNextManifest("foo", testCase.expectedTriggerMode)
		})
	}
}

func TestDCResourceNoImage(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.setupFoo()
	f.file("docker-compose.yml", simpleConfig)
	f.file("Tiltfile", `
docker_compose('docker-compose.yml')
dc_resource('foo', trigger_mode=TRIGGER_MODE_AUTO)
`)

	f.load()
}

func TestDCDependsOn(t *testing.T) {
	f := newFixture(t)
	defer f.TearDown()

	f.dockerfile(filepath.Join("foo", "Dockerfile"))
	f.file("docker-compose.yml", twoServiceConfig)
	f.file("Tiltfile", `
docker_compose('docker-compose.yml')
dc_resource('bar', resource_deps=['foo'])
`)

	f.load()
	f.assertNextManifest("foo", resourceDeps())
	f.assertNextManifest("bar", resourceDeps("foo"))
}

func (f *fixture) assertDcManifest(name model.ManifestName, opts ...interface{}) model.Manifest {
	m := f.assertNextManifest(name)

	if !m.IsDC() {
		f.t.Error("expected a docker-compose manifest")
	}
	dcInfo := m.DockerComposeTarget()

	for _, opt := range opts {
		switch opt := opt.(type) {
		case dcConfigPathHelper:
			assert.Equal(f.t, opt.paths, dcInfo.ConfigPaths, "docker compose config path")
		case dcLocalPathsHelper:
			assert.ElementsMatch(f.t, opt.paths, dcInfo.LocalPaths(), "docker compose local paths")
		case dcYAMLRawHelper:
			assert.Equal(f.t, strings.TrimSpace(opt.yaml), strings.TrimSpace(string(dcInfo.YAMLRaw)), "docker compose YAML raw")
		case dcDfRawHelper:
			assert.Equal(f.t, strings.TrimSpace(opt.df), strings.TrimSpace(string(dcInfo.DfRaw)), "docker compose Dockerfile raw")
		case dcPublishedPortsHelper:
			assert.Equal(f.t, opt.ports, dcInfo.PublishedPorts(), "docker compose published ports")
		default:
			f.t.Fatalf("unexpected arg to assertDcManifest: %T %v", opt, opt)
		}
	}
	return m
}

type dcConfigPathHelper struct {
	paths []string
}

func dcConfigPath(paths []string) dcConfigPathHelper {
	return dcConfigPathHelper{paths}
}

type dcYAMLRawHelper struct {
	yaml string
}

func dcYAMLRaw(yaml string) dcYAMLRawHelper {
	return dcYAMLRawHelper{yaml}
}

type dcDfRawHelper struct {
	df string
}

func dcDfRaw(df string) dcDfRawHelper {
	return dcDfRawHelper{df}
}

type dcLocalPathsHelper struct {
	paths []string
}

func dcLocalPaths(paths []string) dcLocalPathsHelper {
	return dcLocalPathsHelper{paths: paths}
}

type dcPublishedPortsHelper struct {
	ports []int
}

func dcPublishedPorts(ports ...int) dcPublishedPortsHelper {
	return dcPublishedPortsHelper{ports: ports}
}
