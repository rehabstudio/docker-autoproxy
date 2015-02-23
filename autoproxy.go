package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strings"
	"text/template"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/fsouza/go-dockerclient"
)

const (
	endpoint         = "unix:///var/run/docker.sock"
	nginxConfigDir   = "/etc/nginx/conf.d"
	nginxHtpasswdDir = "/etc/nginx/htpasswd.d"
)

// containerConfig is a simple struct used to contain context data for use
// when rendering templates
type containerConfig struct {
	Name            string
	VHost           string
	ContainerIP     string
	ContainerPort   string
	SSLCertName     string
	HtpasswdEntries []string
}

// cfWriter defines a function type that is used for writing nginx
// configuration or htpasswd files to disk
type cfWriter func(string, *containerConfig) (bool, error)

// configureAndReload writes configuration and htpasswd files for all running
// containers before reloading nginx's configuration. This is a destructive
// operation as some files may be overwritten and others removed, it is
// important that oneill is configured correctly and has very sensible
// defaults to account for any silliness here.
func configureAndReload(ccs []*containerConfig) error {

	// keep track of whether or not we need to reload the nginx config
	var reloadRequired bool

	// write nginx configuration file for each running container, overwriting
	// old files if necessary.
	changed, err := writeNewFiles(writeNewConfigFile, nginxConfigDir, ccs)
	if err != nil {
		return err
	}
	if changed {
		reloadRequired = true
	}

	// write htpasswd file for each container that requires it, overwriting
	// old files if necessary.
	changed, err = writeNewFiles(writeNewHtpasswdFile, nginxHtpasswdDir, ccs)
	if err != nil {
		return err
	}
	if changed {
		reloadRequired = true
	}

	// remove redundant configuration files from the config directory. Note
	// that this won't immediately disable the old sites as nginx keeps its
	// configuration in memory and only reloads it when asked.
	changed, err = removeOldFiles(nginxConfigDir, ccs)
	if err != nil {
		return err
	}
	if changed {
		reloadRequired = true
	}

	// remove redundant htpasswd files from the htpasswd directory.
	changed, err = removeOldFiles(nginxHtpasswdDir, ccs)
	if err != nil {
		return err
	}
	if changed {
		reloadRequired = true
	}

	// reload nginx's configuration by sending a HUP signal to the master
	// process, this performs a hot-reload without any downtime
	if reloadRequired {
		return reloadNginxConfiguration()
	} else {
		logrus.Debug("Skipped reloading nginx configuration")
	}

	return nil
}

// exitOnError checks that an error is not nil. If the passed value is an
// error, it is logged and the program exits with an error code of 1
func exitOnError(err error, prefix string) {
	if err != nil {
		logrus.WithFields(logrus.Fields{"err": err}).Fatal(prefix)
	}
}

// getExistingcontainers grabs a list of currently active (running or
// otherwise) containers from the docker API, parses them into simple structs
// we can use for generating templates and returns them.
func getExistingContainers(client *docker.Client) ([]*containerConfig, error) {

	apiContainers, err := client.ListContainers(docker.ListContainersOptions{
		All:  false,
		Size: false,
	})
	if err != nil {
		return nil, err
	}

	containers := []*containerConfig{}
	for _, apiContainer := range apiContainers {

		container, err := client.InspectContainer(apiContainer.ID)
		if err != nil {
			logrus.WithFields(logrus.Fields{"err": err}).Warn("Unable to inspect container")
			continue
		}

		// convert the slice of env vars into something more manageable
		env := docker.Env(container.Config.Env)

		// if the container doesn't have a `VIRTUAL_HOST` environment variable
		// then we just skip it since we won't be able to configure it properly.
		vHost, hasVHost := env.Map()["VIRTUAL_HOST"]
		if !hasVHost {
			logrus.WithFields(logrus.Fields{
				"container": strings.TrimLeft(apiContainer.Names[0], "/"),
			}).Debug("container does not have a `VIRTUAL_HOST` env variable, skipping")
			continue
		}

		// use the `VIRTUAL_PORT` env var if set. If this variable is not set
		// and the container only exposes a single port then we just fall back
		// to that. If a container exposes multiple ports but doesn't set the
		// `VIRTUAL_PORT` variable we are unable to configure the container
		// and will skip it.
		vPort, hasVPort := env.Map()["VIRTUAL_PORT"]
		if !hasVPort {
			if len(container.NetworkSettings.Ports) > 1 {
				logrus.WithFields(logrus.Fields{
					"container": strings.TrimLeft(apiContainer.Names[0], "/"),
				}).Debug("container does not have a `VIRTUAL_PORT` env variable and exposes more than one port, skipping")
				continue
			} else if len(container.NetworkSettings.Ports) == 0 {
				logrus.WithFields(logrus.Fields{
					"container": strings.TrimLeft(apiContainer.Names[0], "/"),
				}).Debug("container does not expose any ports, skipping")
				continue
			}
			// even though this for loop might look odd, i'm not sure of a
			// better way to extract the key, and we can always be sure
			// there's only one port to iterate over thanks to the clauses
			// above.
			for k, _ := range container.NetworkSettings.Ports {
				vPort = k.Port()
			}
		}

		// if the container doesn't have a `SSL_CERT_NAME` environment variable
		// then we can still configure it, but won't be able to use secure its
		// traffic using HTTPS.
		sslCertName := env.Get("SSL_CERT_NAME")

		// ensure that the cert and key actually exist as if either of these
		// are missing nginx will refuse to start
		certPath := fmt.Sprintf("/etc/nginx/ssl.d/%s.crt", sslCertName)
		if _, err := os.Stat(certPath); len(sslCertName) > 0 && os.IsNotExist(err) {
			logrus.WithFields(logrus.Fields{
				"SSL_CERT_NAME": sslCertName,
				"container":     strings.TrimLeft(apiContainer.Names[0], "/"),
			}).Warning("Unable to find SSL certificate file, disabling HTTPS")
			sslCertName = ""
		}
		keyPath := fmt.Sprintf("/etc/nginx/ssl.d/%s.key", sslCertName)
		if _, err := os.Stat(keyPath); len(sslCertName) > 0 && os.IsNotExist(err) {
			logrus.WithFields(logrus.Fields{
				"SSL_CERT_NAME": sslCertName,
				"container":     strings.TrimLeft(apiContainer.Names[0], "/"),
			}).Warning("Unable to find SSL private key file, disabling HTTPS")
			sslCertName = ""
		}

		// extract any htpasswd entries from the environment (if configured)
		htpasswdEntries := &[]string{}
		err = env.GetJSON("HTPASSWD", htpasswdEntries)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"HTPASSWD":  env.Get("HTPASSWD"),
				"container": strings.TrimLeft(apiContainer.Names[0], "/"),
			}).Debug("Unable to parse htpasswd entries from container, is `HTPASSWD` a JSON array?")
		}

		cc := &containerConfig{
			Name:            strings.TrimLeft(apiContainer.Names[0], "/"),
			VHost:           vHost,
			ContainerIP:     container.NetworkSettings.IPAddress,
			ContainerPort:   vPort,
			SSLCertName:     sslCertName,
			HtpasswdEntries: *htpasswdEntries,
		}

		containers = append(containers, cc)
	}
	return containers, nil

}

// main runs docker-autoproxy's main loop, polling the docker api for
// container details every 5 seconds.
func main() {

	cliLogLevel := parseCliArgs()
	logLevel, err := logrus.ParseLevel(cliLogLevel)
	exitOnError(err, "Unable to initialise logger")

	// configure global logger instance
	logrus.SetLevel(logLevel)

	// connect to docker api and initialise a new client
	client, err := docker.NewClient(endpoint)
	exitOnError(err, "Unable to connect to docker API")

	for {
		// grab a current list of all active containers from the docker api
		containers, err := getExistingContainers(client)
		exitOnError(err, "Unable to fetch container details")

		// reconfigure nginx as appropriate
		err = configureAndReload(containers)
		exitOnError(err, "Unable to configure and reload nginx")

		// sleep for a few seconds before starting the polling loop all over
		// again
		time.Sleep(5 * time.Second)
	}

}

// parseCliArgs parses any arguments passed to docker-autoproxy on the command line
func parseCliArgs() string {

	// parse log level from command line (default: info)
	logLevel := flag.String("loglevel", "info", "docker-autoproxy logging level (use \"debug\" for verbose output)")
	flag.Parse()

	return *logLevel
}

// reloadNginxConfiguration issues a `service nginx reload` which causes nginx
// to re-read all of it's configuration files and perform a hot reload. Since
// only root can call this command we use sudo with the `-n` flag, this means
// the the user running oneill is required to have the permission to run this
// command using sudo *without* a password.
func reloadNginxConfiguration() error {

	runCmd := exec.Command("nginx", "-s", "reload")
	output, err := runCmd.CombinedOutput()
	if err != nil {
		return err
	}

	// for some reason when `service nginx reload` fails on ubuntu it returns
	// with an exit code of 0. This means we need to parse the commands output
	// to check if it actually failed or not.
	if strings.Contains(string(output[:]), "fail") {
		return errors.New("Failed to reload nginx")
	}

	logrus.Info("Reloaded nginx configuration")
	return nil
}

// removeIfRedundant checks the given file against a list of currently running
// containers, removing it if a match is not found.
func removeIfRedundant(directory string, f os.FileInfo, rcs []*containerConfig) (bool, error) {

	// if filename matches the name of a currently running container then we
	// just return immediately and skip it.
	for _, rc := range rcs {
		if f.Name() == rc.Name {
			return false, nil
		}
	}

	filePath := path.Join(directory, f.Name())
	logrus.WithFields(logrus.Fields{"filePath": filePath}).Info("Removing file")
	return true, os.Remove(filePath)
}

// removeOldFiles scans a local directory, removing any files where the
// filename does not match the name of a currently running container.
func removeOldFiles(directory string, ccs []*containerConfig) (bool, error) {

	var removedFiles bool

	// scan the configured directory, erroring if we don't have permission, it
	// doesn't exist, etc.
	dirContents, err := ioutil.ReadDir(directory)
	if err != nil {
		return false, err
	}

	// loop over all files in the directory checking each one against our
	// currently running list of containers. If the file doesn't match a
	// running container then we delete it.
	for _, f := range dirContents {
		removedFile, err := removeIfRedundant(directory, f, ccs)
		if err != nil {
			return false, err
		}
		if removedFile {
			removedFiles = true
		}
	}

	return removedFiles, nil
}

// writeIfChanged writes the given `content` to disk at `path` if the file
// does not already exist. If the file does already exist then it will only be
// written to if the content is different from what's on disk.
func writeIfChanged(path string, content []byte) (bool, error) {

	var fileExists bool
	var contentChanged bool

	if _, err := os.Stat(path); err == nil {
		fileExists = true

		readContent, err := ioutil.ReadFile(path)
		if err != nil {
			return false, err
		}

		if !bytes.Equal(content, readContent) {
			contentChanged = true
		}
	}

	if !fileExists || contentChanged {
		logrus.WithFields(logrus.Fields{"filePath": path}).Info("Writing file")
		return true, ioutil.WriteFile(path, content, 0644)
	}

	return false, nil
}

// writeNewConfigFile writes a new nginx configuration file to disk for the
// given container configuration. A simple template file is read from disk at
// runtime. A new file will only be written if the file either doesn't exist
// or its contents have changed.
func writeNewConfigFile(d string, cc *containerConfig) (bool, error) {

	// load configuration file template so we can render it
	nginxTemplate, err := template.ParseFiles("autoproxy.tmpl")
	if err != nil {
		return false, err
	}

	// build template context and render the template to `b`
	var b bytes.Buffer
	if nginxTemplate.Execute(&b, cc) != nil {
		logrus.WithFields(logrus.Fields{
			"container": cc.Name,
		}).Warn("Unspecified error whilst rendering configuration template")
		return false, nil
	}

	// write rendered template to disk
	configFilePath := path.Join(d, cc.Name)
	return writeIfChanged(configFilePath, b.Bytes())
}

// writeNewFiles writes a file to disk for each configured container using the
// passed in function. writeNewFiles first ensures that the directory into
// which the files will be written has been created.
func writeNewFiles(f cfWriter, d string, ccs []*containerConfig) (bool, error) {

	var wroteFiles bool

	// create directory to store config/htpasswd files
	err := os.MkdirAll(d, 0755)
	if err != nil {
		return false, err
	}

	// loop over and write a configuration file for every running container
	for _, cc := range ccs {
		// call the passed in cfWriter function on each container
		wroteFile, err := f(d, cc)
		if err != nil {
			return false, err
		}
		if wroteFile {
			wroteFiles = true
		}
	}
	return wroteFiles, nil
}

// writeNewHtpasswdFile writes a htpasswd file to disk if required. A new file
// will only be written if the file either doesn't exist or its contents have
// changed.
func writeNewHtpasswdFile(d string, cc *containerConfig) (bool, error) {

	// check if we need to write a htpasswd file or not
	if len(cc.HtpasswdEntries) == 0 {
		return false, nil
	}

	// write htpasswd file to disk
	fileContent := []byte(strings.Join(cc.HtpasswdEntries, "\n"))
	return writeIfChanged(path.Join(d, cc.Name), fileContent)
}
