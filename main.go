// Example to demonstrate helm chart installation using helm client-go
// Most of the code is copied from https://github.com/helm/helm repo

package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/docker/docker/api/types"
	"github.com/gofrs/flock"
	"github.com/pkg/errors"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/cli/values"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/repo"
	"helm.sh/helm/v3/pkg/strvals"

	"bufio"

	"github.com/docker/docker/client"
)

type InfoImagen struct {
	Nombre        string
	TotalSize     int64
	CantidadCapas int
}

var settings *cli.EnvSettings

var (
	url         = "https://charts.helm.sh/stable"
	repoName    = "stable"
	chartName   = "mysql"
	releaseName = "mysql-dev"
	namespace   = "mysql-test"
	args        = map[string]string{
		// comma seperated values to set
		"set": "mysqlRootPassword=admin@123,persistence.enabled=false,imagePullPolicy=Always",
	}
)

func main() {
	os.Setenv("HELM_NAMESPACE", namespace)
	settings = cli.New()
	// Add helm repo
	RepoAdd(repoName, url)
	// Update charts from the helm repo
	RepoUpdate()
	// Install charts
	manifest := InstallChart(releaseName, repoName, chartName, args)

	images := ExtractImagesFromManifests(manifest)

	for _, imgs := range images {
		ImagePull(imgs)
		LayersSizeImages(imgs)
	}
}

// RepoAdd adds repo with given name and url
func RepoAdd(name, url string) {
	repoFile := settings.RepositoryConfig

	//Ensure the file directory exists as it is required for file locking
	err := os.MkdirAll(filepath.Dir(repoFile), os.ModePerm)
	if err != nil && !os.IsExist(err) {
		log.Fatal(err)
	}

	// Acquire a file lock for process synchronization
	fileLock := flock.New(strings.Replace(repoFile, filepath.Ext(repoFile), ".lock", 1))
	lockCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	locked, err := fileLock.TryLockContext(lockCtx, time.Second)
	if err == nil && locked {
		defer fileLock.Unlock()
	}
	if err != nil {
		log.Fatal(err)
	}

	b, err := ioutil.ReadFile(repoFile)
	if err != nil && !os.IsNotExist(err) {
		log.Fatal(err)
	}

	var f repo.File
	if err := yaml.Unmarshal(b, &f); err != nil {
		log.Fatal(err)
	}

	if f.Has(name) {
		fmt.Printf("repository name (%s) already exists\n", name)
		return
	}

	c := repo.Entry{
		Name: name,
		URL:  url,
	}

	r, err := repo.NewChartRepository(&c, getter.All(settings))
	if err != nil {
		log.Fatal(err)
	}

	if _, err := r.DownloadIndexFile(); err != nil {
		err := errors.Wrapf(err, "looks like %q is not a valid chart repository or cannot be reached", url)
		log.Fatal(err)
	}

	f.Update(&c)

	if err := f.WriteFile(repoFile, 0644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%q has been added to your repositories\n", name)
}

// RepoUpdate updates charts for all helm repos
func RepoUpdate() {
	repoFile := settings.RepositoryConfig

	f, err := repo.LoadFile(repoFile)
	if os.IsNotExist(errors.Cause(err)) || len(f.Repositories) == 0 {
		log.Fatal(errors.New("no repositories found. You must add one before updating"))
	}
	var repos []*repo.ChartRepository
	for _, cfg := range f.Repositories {
		r, err := repo.NewChartRepository(cfg, getter.All(settings))
		if err != nil {
			log.Fatal(err)
		}
		repos = append(repos, r)
	}

	fmt.Printf("Hang tight while we grab the latest from your chart repositories...\n")
	var wg sync.WaitGroup
	for _, re := range repos {
		wg.Add(1)
		go func(re *repo.ChartRepository) {
			defer wg.Done()
			if _, err := re.DownloadIndexFile(); err != nil {
				fmt.Printf("...Unable to get an update from the %q chart repository (%s):\n\t%s\n", re.Config.Name, re.Config.URL, err)
			} else {
				fmt.Printf("...Successfully got an update from the %q chart repository\n", re.Config.Name)
			}
		}(re)
	}
	wg.Wait()
	fmt.Printf("Update Complete. ⎈ Happy Helming!⎈\n")
}

// InstallChart
func InstallChart(name, repo, chart string, args map[string]string) string {
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(settings.RESTClientGetter(), settings.Namespace(), os.Getenv("HELM_DRIVER"), debug); err != nil {
		log.Fatal(err)
	}
	client := action.NewInstall(actionConfig)

	if client.Version == "" && client.Devel {
		client.Version = ">0.0.0-0"
	}
	//name, chart, err := client.NameAndChart(args)
	client.ReleaseName = name
	cp, err := client.ChartPathOptions.LocateChart(fmt.Sprintf("%s/%s", repo, chart), settings)
	if err != nil {
		log.Fatal(err)
	}

	debug("CHART PATH: %s\n", cp)

	p := getter.All(settings)
	valueOpts := &values.Options{}
	vals, err := valueOpts.MergeValues(p)
	if err != nil {
		log.Fatal(err)
	}

	// Add args
	if err := strvals.ParseInto(args["set"], vals); err != nil {
		log.Fatal(errors.Wrap(err, "failed parsing --set data"))
	}

	// Check chart dependencies to make sure all are present in /charts
	chartRequested, err := loader.Load(cp)
	if err != nil {
		log.Fatal(err)
	}

	validInstallableChart, err := isChartInstallable(chartRequested)
	if !validInstallableChart {
		log.Fatal(err)
	}

	if req := chartRequested.Metadata.Dependencies; req != nil {
		// If CheckDependencies returns an error, we have unfulfilled dependencies.
		// As of Helm 2.4.0, this is treated as a stopping condition:
		// https://github.com/helm/helm/issues/2209
		if err := action.CheckDependencies(chartRequested, req); err != nil {
			if client.DependencyUpdate {
				man := &downloader.Manager{
					Out:              os.Stdout,
					ChartPath:        cp,
					Keyring:          client.ChartPathOptions.Keyring,
					SkipUpdate:       false,
					Getters:          p,
					RepositoryConfig: settings.RepositoryConfig,
					RepositoryCache:  settings.RepositoryCache,
				}
				if err := man.Update(); err != nil {
					log.Fatal(err)
				}
			} else {
				log.Fatal(err)
			}
		}
	}

	client.Namespace = settings.Namespace()
	release, err := client.Run(chartRequested, vals)
	if err != nil {
		log.Fatal(err)
	}
	return release.Manifest
}

func isChartInstallable(ch *chart.Chart) (bool, error) {
	switch ch.Metadata.Type {
	case "", "application":
		return true, nil
	}
	return false, errors.Errorf("%s charts are not installable", ch.Metadata.Type)
}

func debug(format string, v ...interface{}) {
	format = fmt.Sprintf("[debug] %s\n", format)
	log.Output(2, fmt.Sprintf(format, v...))
}

// Extrae todas las imágenes encontradas en documentos YAML
func ExtractImagesFromManifests(manifests string) []string {
	var images []string

	scanner := bufio.NewScanner(strings.NewReader(manifests))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "image:") {
			// asegurarse de no incluir comentarios o líneas incompletas
			parts := strings.SplitN(line, "image:", 2)
			if len(parts) == 2 {
				imgs := strings.TrimSpace(parts[1])
				// evitar capturar valores vacíos o comentados
				if imgs != "" && !strings.HasPrefix(imgs, "#") {
					images = append(images, imgs)
				}
			}
		}
	}
	return images
}

func ImagePull(nombreImagen string) error {
	ctx := context.Background()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("error creando cliente Docker: %w", err)
	}

	defer cli.Close()
	imgs, _ := strconv.Unquote(nombreImagen)
	events, err := cli.ImagePull(
		ctx,
		imgs,
		types.ImagePullOptions{},
	)

	if err != nil {
		panic(err)
	}
	defer events.Close()

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func(wg *sync.WaitGroup) {
		time.Sleep(1 * time.Second)
		list, err := cli.ImageList(ctx, types.ImageListOptions{})
		fmt.Printf("The list is:", list)
		fmt.Println(reflect.TypeOf(list))
		if err != nil {
			panic(err)
		}
		for _, ims := range list {
			fmt.Printf("The ims is:", ims)
			for _, tag := range ims.RepoTags {
				fmt.Printf("The tag is:", tag)
				if strings.Contains(tag, nombreImagen) {
					fmt.Println("Is working")
					wg.Done()
				}
			}
		}
	}(&wg)
	wg.Wait()
	return nil
}

func LayersSizeImages(nombreImagen string) error {

	ctx := context.Background()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("error creando cliente Docker: %w", err)
	}

	imgs, _ := strconv.Unquote(nombreImagen)
	history, err := cli.ImageHistory(ctx, imgs)

	var total int64
	for _, layer := range history {
		total += layer.Size
	}

	info := &InfoImagen{
		Nombre:        nombreImagen,
		TotalSize:     total,
		CantidadCapas: len(history),
	}

	fmt.Printf("Image: %s\n", info.Nombre)
	fmt.Printf("Number of layers: %d\n", info.CantidadCapas)
	fmt.Printf("Total size layers: %d bytes (%.2f MB)\n",
		info.TotalSize, float64(info.TotalSize)/1024.0/1024.0)

	if err != nil {
		panic(err)
	}

	//base := strings.Split(imgs, ":")[0]
	return nil

}
