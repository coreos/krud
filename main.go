package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coreos/pkg/flagutil"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client"
	"k8s.io/kubernetes/pkg/kubectl"
)

func main() {
	fs := flag.NewFlagSet("krud", flag.ExitOnError)
	listen := fs.String("listen", ":9500", "")
	deploymentKey := fs.String("deployment-key", "deployment", "Key to use to differentiate between two different controllers.")
	controllerName := fs.String("controller-name", "", "Name of the replication controller to update.")
	namespace := fs.String("namespace", "", "Namespace the replicationController belongs to.")
	k8sEndpoint := fs.String("k8s-endpoint", "http://localhost:8080", "URL of the Kubernetes API server")

	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	if err := flagutil.SetFlagsFromEnv(fs, "KRUD"); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	if *deploymentKey == "" || *controllerName == "" {
		panic("missing deployment key or controller name")
	}

	k := &Krud{
		DeploymentKey:  *deploymentKey,
		ControllerName: *controllerName,
		Endpoint:       *k8sEndpoint,
		Namespace:      *namespace,
	}

	http.HandleFunc("/push", k.push)
	http.HandleFunc("/", k.view)
	log.Fatal(k.listen(*listen))
}

type Krud struct {
	DeploymentKey  string
	ControllerName string
	Namespace      string
	Endpoint       string

	// Hooks contains all incoming webhooks
	Hooks []*Webhook
	// Next is the next-to-update webhook, nil for none. Multiple attempts will
	// use the most recently received hook.
	Next chan *Webhook

	sync.Mutex
}

func (k *Krud) listen(listen string) error {
	k.Next = make(chan *Webhook)
	go k.start()
	log.Println("serving on", listen)
	return http.ListenAndServe(listen, nil)
}

func (k *Krud) start() {
	for {
		h := <-k.Next
	Loop:
		for {
			select {
			case c := <-k.Next:
				if h.Received.Before(c.Received) {
					h = c
				}
			default:
				break Loop
			}
		}
		if err := k.update(h); err != nil {
			k.Lock()
			h.UpdateError = err
			k.Unlock()
		}
	}
}

type Webhook struct {
	Data     interface{}
	Kind     string
	Source   string
	Received time.Time

	// UpdateAttempt is set if an update was started for this hook.
	UpdateAttempt bool
	// UpdateID is the ID of this update, which is what the value of the deployment
	// key is set to.
	UpdateID string
	// UpdateSuccess is true if the attempted update was successful.
	UpdateSuccess bool
	UpdateStart   time.Time
	UpdateEnd     time.Time
	UpdateStatus  string
	UpdateError   error
}

func serveError(w http.ResponseWriter, err error) {
	log.Println(err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

type QuayWebhook struct {
	DockerURL        string `json:"docker_url"`
	Homepage         string
	Name             string
	Namespace        string
	PrunedImageCount int `json:"pruned_image_count"`
	PushedImageCount int `json:"pushed_image_count"`
	Repository       string
	UpdatedTags      struct {
		Latest string
	} `json:"updated_tags"`
	Visibility string
}

type DockerWebhook struct {
	CallbackURL string `json:"callback_url"`
	PushData    struct {
		Images   interface{} `json:"images"`
		PushedAt int         `json:"pushed_at"`
		Pusher   string      `json:"pusher"`
	} `json:"push_data"`
	Repository struct {
		CommentCount    int    `json:"comment_count"`
		DateCreated     int    `json:"date_created"`
		Description     string `json:"description"`
		FullDescription string `json:"full_description"`
		IsOfficial      bool   `json:"is_official"`
		IsPrivate       bool   `json:"is_private"`
		IsTrusted       bool   `json:"is_trusted"`
		Name            string `json:"name"`
		Namespace       string `json:"namespace"`
		Owner           string `json:"owner"`
		RepoName        string `json:"repo_name"`
		RepoURL         string `json:"repo_url"`
		StarCount       int    `json:"star_count"`
		Status          string `json:"status"`
	} `json:"repository"`
}

var (
	viewFuncs = template.FuncMap{
		"json": func(v interface{}) (string, error) {
			b, err := json.MarshalIndent(v, "", "  ")
			return string(b), err
		},
	}
	viewTemplate = template.Must(template.New("").Funcs(viewFuncs).Parse(indexHTML))
)

func (k *Krud) view(w http.ResponseWriter, r *http.Request) {
	k.Lock()
	defer k.Unlock()
	err := viewTemplate.Execute(w, k)
	if err != nil {
		fmt.Println(err)
		serveError(w, err)
	}
}

func parseWebhook(r io.Reader) (v interface{}, kind string, err error) {
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, "", err
	}
	{
		var d QuayWebhook
		if err := json.Unmarshal(b, &d); err == nil {
			return d, "quay.io", nil
		}
	}
	{
		var d DockerWebhook
		if err := json.Unmarshal(b, &d); err == nil {
			return d, "docker hub", nil
		}
	}
	return nil, "", fmt.Errorf("unrecognized webhook")
}

func (k *Krud) push(w http.ResponseWriter, r *http.Request) {
	d, kind, err := parseWebhook(r.Body)
	if err != nil {
		serveError(w, err)
		return
	}
	k.Lock()
	defer k.Unlock()
	wh := &Webhook{
		Data:     &d,
		Kind:     kind,
		Source:   r.RemoteAddr,
		Received: time.Now(),
	}
	k.Hooks = append(k.Hooks, wh)
	go func() {
		k.Next <- wh
	}()
}

func (k *Krud) update(h *Webhook) error {
	h.UpdateAttempt = true
	h.UpdateStart = time.Now()
	defer func() {
		h.UpdateEnd = time.Now()
	}()
	conf := &client.Config{
		Host: k.Endpoint,
	}
	client, err := client.New(conf)
	if err != nil {
		return err
	}
	if k.Namespace == "" {
		k.Namespace = api.NamespaceDefault
	}
	rcs := client.ReplicationControllers(k.Namespace)
	oldRc, err := rcs.Get(k.ControllerName)
	if err != nil {
		return err
	}
	newRc, err := rcs.Get(k.ControllerName)
	if err != nil {
		return err
	}
	hash, err := api.HashObject(oldRc, client.Codec)
	if err != nil {
		return err
	}
	h.UpdateID = hash
	newRc.Name = fmt.Sprintf("%s-%s", k.ControllerName, hash)
	newRc.ResourceVersion = ""
	apply := func(key, value string, ms ...map[string]string) {
		for _, m := range ms {
			m[key] = value
		}
	}
	apply(k.DeploymentKey, hash, newRc.Spec.Selector, newRc.Spec.Template.Labels)
	apply("run", k.ControllerName, newRc.Spec.Selector, newRc.Spec.Template.Labels)
	ruconf := kubectl.RollingUpdaterConfig{
		Out: &lockBuffer{
			k: k,
			h: h,
		},
		OldRc:          oldRc,
		NewRc:          newRc,
		UpdatePeriod:   time.Second * 3, // todo: change to time.Minute
		Timeout:        time.Minute * 5,
		Interval:       time.Second * 3,
		UpdateAcceptor: kubectl.DefaultUpdateAcceptor,
		CleanupPolicy:  kubectl.RenameRollingUpdateCleanupPolicy,
	}
	ruc := kubectl.NewRollingUpdaterClient(client)
	println("doing rolling update")
	err = kubectl.NewRollingUpdater(k.Namespace, ruc).Update(&ruconf)
	println("done")
	k.Lock()
	h.UpdateSuccess = err == nil
	k.Unlock()
	return err
}

type lockBuffer struct {
	k *Krud
	h *Webhook
}

func (l *lockBuffer) Write(p []byte) (n int, err error) {
	l.k.Lock()
	defer l.k.Unlock()
	l.h.UpdateStatus += string(p)
	fmt.Println("WRITE", string(p))
	return len(p), nil
}

const indexHTML = `<!DOCTYPE html>
<html lang="en">
	<head>
		<meta charset="utf-8">
		<title>krud</title>
	</head>
	<body>
		{{range .Hooks}}
			<div>
				Err: {{.UpdateError}}
				<br>Status: <pre>{{.UpdateStatus}}</pre>
				<br>Value: <pre>{{. | json}}</pre>
			</div>
			<hr>
		{{end}}
	</body>
</html>
`
