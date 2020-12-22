package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
	parsetorrentname "github.com/middelink/go-parse-torrent-name"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
	"github.com/slaveofcode/voodio/collections"
	"github.com/slaveofcode/voodio/logger"
	"github.com/slaveofcode/voodio/repository"
	"github.com/slaveofcode/voodio/repository/models"
	"github.com/slaveofcode/voodio/web"
	"github.com/slaveofcode/voodio/web/config"
)

const (
	appDirName = "voodioapp"
	dbFileName = "voodio.db"
)

var cacheDir, _ = os.UserCacheDir()

func getAppDir() string {
	return filepath.Join(cacheDir, appDirName)
}

func getDBPath() string {
	return filepath.Join(getAppDir(), dbFileName)
}

func init() {
	// First of all, logger...
	logger.Setup()

	// create working app dir
	appDirPath := getAppDir()
	if _, err := os.Stat(appDirPath); os.IsNotExist(err) {
		err = os.MkdirAll(appDirPath, 0777)
		if err != nil {
			panic("Unable to create App Dir on " + appDirPath)
		}
		log.Infoln("Created App dir at", appDirPath)
	}

	// remove old database if exist
	dbPath := getDBPath()
	_, err := os.Stat(dbPath)
	if !os.IsNotExist(err) {
		log.Infoln("Obsolete DB detected, removing...")
		if err = os.Remove(dbPath); err != nil {
			panic("Unable removing obsolete DB")
		}
	}

	_, err = os.Create(dbPath)
	if err != nil {
		log.Errorln("Unable to init db file", err)
		os.Exit(1)
	}

	log.Infoln("DB initialized at", dbPath)
}

func cleanup() {
	log.Infoln("Cleaning up artifacts")
	os.RemoveAll(getAppDir())
}

type resolutionParam []string

func (r *resolutionParam) String() string {
	return ""
}

func (r *resolutionParam) Set(param string) error {
	*r = append(*r, param)
	return nil
}

func main() {
	parentMoviePath := flag.String("path", "", "Path string of parent movie directory")
	serverPort := flag.Int("port", 1818, "Server port number")
	tmdbAPIKey := flag.String("tmdb-key", "", "Your TMDB Api Key, get here if you don't have one https://www.themoviedb.org/documentation/api")

	screenRes := resolutionParam{}
	flag.Var(&screenRes, "resolution", "Specific resolution to be processed: 360p, 480p, 720p and 1080p, this could be multiple")

	flag.Parse()

	if len(screenRes) == 0 {
		screenRes = resolutionParam{
			"360p",
			"480p",
			"720p",
			"1080p",
		}
	}

	if len(*parentMoviePath) == 0 {
		cleanup()
		panic("No movie path directory provided, exited")
	}

	//Check FFmpeg installed or setup in a right way
	if !checkFfmpegInstalled() {
		panic("sorry, you haven't install ffmpeg, INSTALL FFMPEG first!")
	}

	if len(strings.TrimSpace(*tmdbAPIKey)) == 0 {
		cleanup()
		panic("No TMDB Api Key provided, exited")
	}

	dbConn, err := repository.OpenDB(getDBPath())
	if err != nil {
		cleanup()
		panic("Unable to create DB connection:" + err.Error())
	}

	defer dbConn.Close()

	log.Infoln("Preparing database...")
	repository.Migrate(dbConn)
	log.Infoln("Database prepared")

	// Scan movies inside given path
	log.Infoln("Scanning movies...")
	movies, subs, err := collections.ScanDir(*parentMoviePath)
	if err != nil {
		cleanup()
		panic("Error while scanning movies " + err.Error())
	}
	log.Infoln("Scanning movies finished")

	saveMovies(dbConn, movies)
	saveSubs(dbConn, subs)

	// Find duplicate directory names, kind of serial movie
	var movieGroups []models.Movie
	dbConn.Table("movies").
		Select("dir_name, dir_path, COUNT(*) count").
		Group("dir_name, dir_path").
		Having("count > ?", 1).
		Find(&movieGroups)

	for _, mg := range movieGroups {
		// find related movie with same dir_name & dir_path
		var movieList []models.Movie
		dbConn.Where(&models.Movie{
			DirName: mg.DirName,
			DirPath: mg.DirPath,
		}).Find(&movieList)

		for _, m := range movieList {
			dbConn.Model(&m).Update(&models.Movie{
				IsGroupDir: true,
			})
		}
	}

	// create simple webserver
	webServer := web.NewServer(&config.ServerConfig{
		DB:                dbConn,
		Port:              *serverPort,
		AppDir:            getAppDir(),
		TMDBApiKey:        *tmdbAPIKey,
		ScreenResolutions: screenRes,
	})

	closeSignal := make(chan os.Signal, 1)
	signal.Notify(closeSignal, os.Interrupt)

	serverDone := make(chan bool)

	go func() {
		<-closeSignal
		log.Infoln("Shutting down...")

		// Waiting for current process server to finish with 30 secs timeout
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()

		webServer.SetKeepAlivesEnabled(false)
		if err := webServer.Shutdown(ctx); err != nil {
			log.Errorln("Couldn't gracefully shutdown")
		}

		serverDone <- true
	}()

	log.Infoln("Activate API Server")
	log.Infoln("Server is alive")
	showIPServer(*serverPort)
	if err = webServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Errorln("Unable to start server on port", *serverPort)
	}

	<-serverDone

	cleanup()

	log.Infoln("Server closed")
}

func showIPServer(port int) {
	addrs, _ := net.InterfaceAddrs()

	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ipnet.IP.To4() != nil {
				log.Infoln("http://" + ipnet.IP.String() + ":" + strconv.Itoa(port))
			}
		}
	}
}

func saveMovies(dbConn *gorm.DB, movies []collections.MovieDirInfo) {
	for _, movie := range movies {
		dirName := filepath.Base(movie.Dir)
		dirNameParsedInfo, err := parsetorrentname.Parse(filepath.Base(movie.Dir))
		cleanDirName := ""
		if err == nil {
			cleanDirName = dirNameParsedInfo.Title
		}

		baseNameParsedInfo, _ := parsetorrentname.Parse(movie.MovieFile)
		cleanBaseName := ""
		if err == nil {
			cleanBaseName = baseNameParsedInfo.Title
		}

		dbConn.Create(&models.Movie{
			DirPath:       movie.Dir,
			DirName:       dirName,
			CleanDirName:  cleanDirName,
			FileSize:      movie.MovieSize,
			BaseName:      movie.MovieFile,
			CleanBaseName: cleanBaseName,
			MimeType:      movie.MimeType,
			IsGroupDir:    false,
			IsPrepared:    false,
		})
	}
}

func saveSubs(dbConn *gorm.DB, subs []collections.SubDirInfo) {
	for _, sub := range subs {
		dirName := filepath.Base(sub.Dir)
		dirNameParsedInfo, err := parsetorrentname.Parse(filepath.Base(sub.Dir))
		cleanDirName := ""
		if err == nil {
			cleanDirName = dirNameParsedInfo.Title
		}

		baseNameParsedInfo, _ := parsetorrentname.Parse(sub.SubFile)
		cleanBaseName := ""
		if err == nil {
			cleanBaseName = baseNameParsedInfo.Title
		}

		dbConn.Create(&models.Subtitle{
			DirPath:       sub.Dir,
			DirName:       dirName,
			CleanDirName:  cleanDirName,
			BaseName:      sub.SubFile,
			CleanBaseName: cleanBaseName,
		})
	}
}

func checkFfmpegInstalled() bool {
	_, err := exec.LookPath("ffmpeg")
	if err != nil {
		logrus.Errorln("error: ", err)
		return false
	}
	return true
}
