package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	service "github.com/neunhoef/ArangoDBStarter/service"
	logging "github.com/op/go-logging"
	"github.com/spf13/cobra"
)

// Configuration data with defaults:

const (
	projectName          = "arangodb"
	defaultDockerGCDelay = time.Minute * 10
)

var (
	projectVersion = "dev"
	projectBuild   = "dev"
	cmdMain        = cobra.Command{
		Use:   projectName,
		Short: "Start ArangoDB clusters with ease",
		Run:   cmdMainRun,
	}
	log               = logging.MustGetLogger(projectName)
	id                string
	agencySize        int
	arangodExecutable string
	arangodJSstartup  string
	masterPort        int
	rrPath            string
	startCoordinator  bool
	startDBserver     bool
	dataDir           string
	ownAddress        string
	masterAddress     string
	verbose           bool
	serverThreads     int
	dockerEndpoint    string
	dockerImage       string
	dockerUser        string
	dockerContainer   string
	dockerGCDelay     time.Duration
	dockerNetHost     bool
	dockerPrivileged  bool
)

func init() {
	f := cmdMain.Flags()
	f.IntVar(&agencySize, "agencySize", 3, "Number of agents in the cluster")
	f.StringVar(&id, "id", "", "Unique identifier of this peer")
	f.StringVar(&arangodExecutable, "arangod", "/usr/sbin/arangod", "Path of arangod")
	f.StringVar(&arangodJSstartup, "jsDir", "/usr/share/arangodb3/js", "Path of arango JS")
	f.IntVar(&masterPort, "masterPort", 4000, "Port to listen on for other arangodb's to join")
	f.StringVar(&rrPath, "rr", "", "Path of rr")
	f.BoolVar(&startCoordinator, "startCoordinator", true, "should a coordinator instance be started")
	f.BoolVar(&startDBserver, "startDBserver", true, "should a dbserver instance be started")
	f.StringVar(&dataDir, "dataDir", getEnvVar("DATA_DIR", "."), "directory to store all data")
	f.StringVar(&ownAddress, "ownAddress", "", "address under which this server is reachable, needed for running arangodb in docker or the case of --agencySize 1 in the master")
	f.StringVar(&masterAddress, "join", "", "join a cluster with master at address addr")
	f.BoolVar(&verbose, "verbose", false, "Turn on debug logging")
	f.IntVar(&serverThreads, "server.threads", 0, "Adjust server.threads of each server")
	f.StringVar(&dockerEndpoint, "dockerEndpoint", "unix:///var/run/docker.sock", "Endpoint used to reach the docker daemon")
	f.StringVar(&dockerImage, "docker", getEnvVar("DOCKER_IMAGE", ""), "name of the Docker image to use to launch arangod instances (leave empty to avoid using docker)")
	f.StringVar(&dockerUser, "dockerUser", "", "use the given name as user to run the Docker container")
	f.StringVar(&dockerContainer, "dockerContainer", "", "name of the docker container that is running this process")
	f.DurationVar(&dockerGCDelay, "dockerGCDelay", defaultDockerGCDelay, "Delay before stopped containers are garbage collected")
	f.BoolVar(&dockerNetHost, "dockerNetHost", false, "Run containers with --net=host")
	f.BoolVar(&dockerPrivileged, "dockerPrivileged", false, "Run containers with --privileged")
}

// handleSignal listens for termination signals and stops this process onup termination.
func handleSignal(sigChannel chan os.Signal, cancel context.CancelFunc) {
	signalCount := 0
	for s := range sigChannel {
		signalCount++
		fmt.Println("Received signal:", s)
		if signalCount > 1 {
			os.Exit(1)
		}
		cancel()
	}
}

// For Windows we need to change backslashes to slashes, strangely enough:
func slasher(s string) string {
	return strings.Replace(s, "\\", "/", -1)
}

func findExecutable() {
	var pathList = make([]string, 0, 10)
	pathList = append(pathList, "build/bin/arangod")
	switch runtime.GOOS {
	case "windows":
		// Look in the default installation location:
		foundPaths := make([]string, 0, 20)
		basePath := "C:/Program Files"
		d, e := os.Open(basePath)
		if e == nil {
			l, e := d.Readdir(1024)
			if e == nil {
				for _, n := range l {
					if n.IsDir() {
						name := n.Name()
						if strings.HasPrefix(name, "ArangoDB3 ") ||
							strings.HasPrefix(name, "ArangoDB3e ") {
							foundPaths = append(foundPaths, basePath+"/"+name+
								"/usr/bin/arangod.exe")
						}
					}
				}
			} else {
				log.Errorf("Could not read directory %s to look for executable.", basePath)
			}
			d.Close()
		} else {
			log.Errorf("Could not open directory %s to look for executable.", basePath)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(foundPaths)))
		pathList = append(pathList, foundPaths...)
	case "darwin":
		pathList = append(pathList,
			"/Applications/ArangoDB3-CLI.app/Contents/MacOS/usr/sbin/arangod",
			"/usr/local/opt/arangodb/sbin/arangod",
		)
	case "linux":
		pathList = append(pathList,
			"/usr/sbin/arangod",
		)
	}
	for _, p := range pathList {
		if _, e := os.Stat(filepath.Clean(filepath.FromSlash(p))); e == nil || !os.IsNotExist(e) {
			arangodExecutable, _ = filepath.Abs(filepath.FromSlash(p))
			if p == "build/bin/arangod" {
				arangodJSstartup, _ = filepath.Abs("js")
			} else {
				arangodJSstartup, _ = filepath.Abs(
					filepath.FromSlash(filepath.Dir(p) + "/../share/arangodb3/js"))
			}
			return
		}
	}
}

func main() {
	// Find executable and jsdir default in a platform dependent way:
	findExecutable()

	cmdMain.Execute()
}

func cmdMainRun(cmd *cobra.Command, args []string) {
	log.Infof("Starting %s version %s, build %s", projectName, projectVersion, projectBuild)

	if verbose {
		logging.SetLevel(logging.DEBUG, projectName)
	} else {
		logging.SetLevel(logging.INFO, projectName)
	}
	// Some plausibility checks:
	if agencySize%2 == 0 || agencySize <= 0 {
		log.Fatal("Error: agencySize needs to be a positive, odd number.")
	}
	if agencySize == 1 && ownAddress == "" {
		log.Fatal("Error: if agencySize==1, ownAddress must be given.")
	}
	if dockerImage != "" && rrPath != "" {
		log.Fatal("Error: using --dockerImage and --rr is not possible.")
	}
	log.Debugf("Using %s as default arangod executable.", arangodExecutable)
	log.Debugf("Using %s as default JS dir.", arangodJSstartup)

	// Sort out work directory:
	if len(dataDir) == 0 {
		dataDir = "."
	}
	dataDir, _ = filepath.Abs(dataDir)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("Cannot create data directory %s because %v, giving up.", dataDir, err)
	}

	// Interrupt signal:
	sigChannel := make(chan os.Signal)
	rootCtx, cancel := context.WithCancel(context.Background())
	signal.Notify(sigChannel, os.Interrupt, syscall.SIGTERM)
	go handleSignal(sigChannel, cancel)

	// Create service
	service, err := service.NewService(log, service.ServiceConfig{
		ID:                id,
		AgencySize:        agencySize,
		ArangodExecutable: arangodExecutable,
		ArangodJSstartup:  arangodJSstartup,
		MasterPort:        masterPort,
		RrPath:            rrPath,
		StartCoordinator:  startCoordinator,
		StartDBserver:     startDBserver,
		DataDir:           dataDir,
		OwnAddress:        ownAddress,
		MasterAddress:     masterAddress,
		Verbose:           verbose,
		ServerThreads:     serverThreads,
		RunningInDocker:   os.Getenv("RUNNING_IN_DOCKER") == "true",
		DockerContainer:   dockerContainer,
		DockerEndpoint:    dockerEndpoint,
		DockerImage:       dockerImage,
		DockerUser:        dockerUser,
		DockerGCDelay:     dockerGCDelay,
		DockerNetHost:     dockerNetHost,
		DockerPrivileged:  dockerPrivileged,
		ProjectVersion:    projectVersion,
		ProjectBuild:      projectBuild,
	})
	if err != nil {
		log.Fatalf("Failed to create service: %#v", err)
	}

	// Run the service
	service.Run(rootCtx)
}

// getEnvVar returns the value of the environment variable with given key of the given default
// value of no such variable exist or is empty.
func getEnvVar(key, defaultValue string) string {
	value := os.Getenv(key)
	if value != "" {
		return value
	}
	return defaultValue
}
