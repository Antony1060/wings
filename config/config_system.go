package config

import (
	"context"
	"fmt"
	"html/template"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/spf13/viper"
)

// Defines basic system configuration settings.
type SystemConfiguration struct {
	// The root directory where all of the pterodactyl data is stored at.
	RootDirectory string `default:"/var/lib/pterodactyl" mapstructre:"root_directory"`

	// Directory where logs for server installations and other wings events are logged.
	LogDirectory string `default:"/var/log/pterodactyl" mapstructre:"log_directory"`

	// Directory where the server data is stored at.
	Data string `default:"/var/lib/pterodactyl/volumes" mapstructre:"data"`

	// Directory where server archives for transferring will be stored.
	ArchiveDirectory string `default:"/var/lib/pterodactyl/archives" mapstructre:"archive_directory"`

	// Directory where local backups will be stored on the machine.
	BackupDirectory string `default:"/var/lib/pterodactyl/backups" mapstructre:"backup_directory"`

	// The user that should own all of the server files, and be used for containers.
	Username string `default:"pterodactyl" mapstructre:"username"`

	// The timezone for this Wings instance. This is detected by Wings automatically if possible,
	// and falls back to UTC if not able to be detected. If you need to set this manually, that
	// can also be done.
	//
	// This timezone value is passed into all containers created by Wings.
	Timezone string `mapstructre:"timezone"`

	// Definitions for the user that gets created to ensure that we can quickly access
	// this information without constantly having to do a system lookup.
	User struct {
		Uid int
		Gid int
	}

	// The amount of time in seconds that can elapse before a server's disk space calculation is
	// considered stale and a re-check should occur. DANGER: setting this value too low can seriously
	// impact system performance and cause massive I/O bottlenecks and high CPU usage for the Wings
	// process.
	//
	// Set to 0 to disable disk checking entirely. This will always return 0 for the disk space used
	// by a server and should only be set in extreme scenarios where performance is critical and
	// disk usage is not a concern.
	DiskCheckInterval int64 `default:"150" mapstructre:"disk_check_interval"`

	// If set to true, file permissions for a server will be checked when the process is
	// booted. This can cause boot delays if the server has a large amount of files. In most
	// cases disabling this should not have any major impact unless external processes are
	// frequently modifying a servers' files.
	CheckPermissionsOnBoot bool `default:"true" mapstructre:"check_permissions_on_boot"`

	// If set to false Wings will not attempt to write a log rotate configuration to the disk
	// when it boots and one is not detected.
	EnableLogRotate bool `default:"true" mapstructre:"enable_log_rotate"`

	// The number of lines to send when a server connects to the websocket.
	WebsocketLogCount int `default:"150" mapstructre:"websocket_log_count"`

	Sftp SftpConfiguration `mapstructre:"sftp"`

	CrashDetection CrashDetection `mapstructre:"crash_detection"`

	Backups Backups `mapstructre:"backups"`

	Transfers Transfers `mapstructre:"transfers"`
}

type CrashDetection struct {
	// Determines if Wings should detect a server that stops with a normal exit code of
	// "0" as being crashed if the process stopped without any Wings interaction. E.g.
	// the user did not press the stop button, but the process stopped cleanly.
	DetectCleanExitAsCrash bool `default:"true" mapstructre:"detect_clean_exit_as_crash"`

	// Timeout specifies the timeout between crashes that will not cause the server
	// to be automatically restarted, this value is used to prevent servers from
	// becoming stuck in a boot-loop after multiple consecutive crashes.
	Timeout int `default:"60" json:"timeout"`
}

type Backups struct {
	// WriteLimit imposes a Disk I/O write limit on backups to the disk, this affects all
	// backup drivers as the archiver must first write the file to the disk in order to
	// upload it to any external storage provider.
	//
	// If the value is less than 1, the write speed is unlimited,
	// if the value is greater than 0, the write speed is the value in MiB/s.
	//
	// Defaults to 0 (unlimited)
	WriteLimit int `default:"0" mapstructre:"write_limit"`
}

type Transfers struct {
	// DownloadLimit imposes a Network I/O read limit when downloading a transfer archive.
	//
	// If the value is less than 1, the write speed is unlimited,
	// if the value is greater than 0, the write speed is the value in MiB/s.
	//
	// Defaults to 0 (unlimited)
	DownloadLimit int `default:"0" mapstructre:"download_limit"`
}

// ConfigureDirectories ensures that all of the system directories exist on the
// system. These directories are created so that only the owner can read the data,
// and no other users.
func ConfigureDirectories() error {
	root := viper.GetString("system.root_directory")
	log.WithField("path", root).Debug("ensuring root data directory exists")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}

	// There are a non-trivial number of users out there whose data directories are actually a
	// symlink to another location on the disk. If we do not resolve that final destination at this
	// point things will appear to work, but endless errors will be encountered when we try to
	// verify accessed paths since they will all end up resolving outside the expected data directory.
	//
	// For the sake of automating away as much of this as possible, see if the data directory is a
	// symlink, and if so resolve to its final real path, and then update the configuration to use
	// that.
	data := viper.GetString("system.data")
	if d, err := filepath.EvalSymlinks(data); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	} else if d != data {
		data = d
		viper.Set("system.data", d)
	}

	log.WithField("path", data).Debug("ensuring server data directory exists")
	if err := os.MkdirAll(data, 0700); err != nil {
		return err
	}

	log.WithField("path", viper.GetString("system.archive_directory")).Debug("ensuring archive data directory exists")
	if err := os.MkdirAll(viper.GetString("system.archive_directory"), 0700); err != nil {
		return err
	}

	log.WithField("path", viper.GetString("system.backup_directory")).Debug("ensuring backup data directory exists")
	if err := os.MkdirAll(viper.GetString("system.backup_directory"), 0700); err != nil {
		return err
	}

	return nil
}

// EnableLogRotation writes a logrotate file for wings to the system logrotate
// configuration directory if one exists and a logrotate file is not found. This
// allows us to basically automate away the log rotation for most installs, but
// also enable users to make modifications on their own.
func EnableLogRotation() error {
	// Do nothing if not enabled.
	if !viper.GetBool("system.enable_log_rotate") {
		log.Info("skipping log rotate configuration, disabled in wings config file")
		return nil
	}

	if st, err := os.Stat("/etc/logrotate.d"); err != nil && !os.IsNotExist(err) {
		return err
	} else if (err != nil && os.IsNotExist(err)) || !st.IsDir() {
		return nil
	}
	if _, err := os.Stat("/etc/logrotate.d/wings"); err == nil || !os.IsNotExist(err) {
		return err
	}

	log.Info("no log rotation configuration found: adding file now")
	// If we've gotten to this point it means the logrotate directory exists on the system
	// but there is not a file for wings already. In that case, let us write a new file to
	// it so files can be rotated easily.
	f, err := os.Create("/etc/logrotate.d/wings")
	if err != nil {
		return err
	}
	defer f.Close()

	type logrotateConfig struct {
		Directory string
		UserID    int
		GroupID   int
	}

	t, err := template.New("logrotate").Parse(`
{{.Directory}}/wings.log {
    size 10M
    compress
    delaycompress
    dateext
    maxage 7
    missingok
    notifempty
    create 0640 {{.UserID}} {{.GroupID}}
    postrotate
        killall -SIGHUP wings
    endscript
}`)
	if err != nil {
		return err
	}

	err = t.Execute(f, logrotateConfig{
		Directory: viper.GetString("system.log_directory"),
		UserID:    viper.GetInt("system.user.uid"),
		GroupID:   viper.GetInt("system.user.gid"),
	})
	return errors.Wrap(err, "config: failed to write logrotate to disk")
}

// Returns the location of the JSON file that tracks server states.
func (sc *SystemConfiguration) GetStatesPath() string {
	return path.Join(sc.RootDirectory, "states.json")
}

// Returns the location of the JSON file that tracks server states.
func (sc *SystemConfiguration) GetInstallLogPath() string {
	return path.Join(sc.LogDirectory, "install/")
}

// ConfigureTimezone sets the timezone data for the configuration if it is
// currently missing. If a value has been set, this functionality will only run
// to validate that the timezone being used is valid.
func ConfigureTimezone() error {
	tz := viper.GetString("system.timezone")
	defer viper.Set("system.timezone", tz)
	if tz == "" {
		b, err := ioutil.ReadFile("/etc/timezone")
		if err != nil {
			if !os.IsNotExist(err) {
				return errors.WithMessage(err, "config: failed to open timezone file")
			}

			tz = "UTC"
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
			defer cancel()
			// Okay, file isn't found on this OS, we will try using timedatectl to handle this. If this
			// command fails, exit, but if it returns a value use that. If no value is returned we will
			// fall through to UTC to get Wings booted at least.
			out, err := exec.CommandContext(ctx, "timedatectl").Output()
			if err != nil {
				log.WithField("error", err).Warn("failed to execute \"timedatectl\" to determine system timezone, falling back to UTC")
				return nil
			}

			r := regexp.MustCompile(`Time zone: ([\w/]+)`)
			matches := r.FindSubmatch(out)
			if len(matches) != 2 || string(matches[1]) == "" {
				log.Warn("failed to parse timezone from \"timedatectl\" output, falling back to UTC")
				return nil
			}
			tz = string(matches[1])
		} else {
			tz = string(b)
		}
	}

	tz = regexp.MustCompile(`(?i)[^a-z_/]+`).ReplaceAllString(tz, "")
	_, err := time.LoadLocation(tz)

	return errors.WithMessage(err, fmt.Sprintf("the supplied timezone %s is invalid", tz))
}
