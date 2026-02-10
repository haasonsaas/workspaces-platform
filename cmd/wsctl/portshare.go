package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
)

func cmdPortShare(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}

	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("portshare create", flag.ExitOnError)
		var (
			name       = fs.String("name", "", "PortShare name (optional; defaults to ps-<desktop>-<port>)")
			namespace  = fs.String("namespace", "desktops", "Namespace")
			desktop    = fs.String("desktop", "", "Desktop name (required)")
			port       = fs.Int("port", 0, "Port to share (required)")
			level      = fs.String("share-level", "owner", "Share level (owner, authenticated, organization, public)")
			proto      = fs.String("protocol", "http", "Protocol hint (http, https, tcp)")
			ttlSeconds = fs.Int("ttl", 0, "TTL seconds (0 = no TTL)")
		)
		fs.Parse(args[1:])

		if strings.TrimSpace(*desktop) == "" || *port <= 0 || *port > 65535 {
			fs.Usage()
			os.Exit(2)
		}
		if *ttlSeconds < 0 {
			die("ttl must be >= 0")
		}

		psName := strings.TrimSpace(*name)
		if psName == "" {
			psName = fmt.Sprintf("ps-%s-%d", strings.TrimSpace(*desktop), *port)
			if len(psName) > 63 {
				psName = psName[:63]
			}
		}

		k, err := newK8sClient()
		dieIf(err)

		spec := workspacesv1alpha1.PortShareSpec{
			DesktopRef: workspacesv1alpha1.PortShareDesktopRef{Name: strings.TrimSpace(*desktop)},
			Port:       int32(*port),
			ShareLevel: workspacesv1alpha1.PortShareLevel(strings.ToLower(strings.TrimSpace(*level))),
			Protocol:   workspacesv1alpha1.PortShareProtocol(strings.ToLower(strings.TrimSpace(*proto))),
			TTLSeconds: int32(*ttlSeconds),
		}

		ps := &workspacesv1alpha1.PortShare{
			ObjectMeta: metav1.ObjectMeta{
				Name:      psName,
				Namespace: strings.TrimSpace(*namespace),
			},
			Spec: spec,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dieIf(k.Create(ctx, ps))

		fmt.Printf("created PortShare %s/%s desktop=%s port=%d\n", ps.Namespace, ps.Name, strings.TrimSpace(*desktop), *port)

	case "get":
		fs := flag.NewFlagSet("portshare get", flag.ExitOnError)
		var (
			name      = fs.String("name", "", "PortShare name")
			namespace = fs.String("namespace", "desktops", "Namespace")
		)
		fs.Parse(args[1:])
		if strings.TrimSpace(*name) == "" {
			fs.Usage()
			os.Exit(2)
		}
		k, err := newK8sClient()
		dieIf(err)
		var ps workspacesv1alpha1.PortShare
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dieIf(k.Get(ctx, client.ObjectKey{Namespace: strings.TrimSpace(*namespace), Name: strings.TrimSpace(*name)}, &ps))
		out, _ := json.MarshalIndent(ps, "", "  ")
		fmt.Println(string(out))

	case "list":
		fs := flag.NewFlagSet("portshare list", flag.ExitOnError)
		var (
			namespace = fs.String("namespace", "desktops", "Namespace")
		)
		fs.Parse(args[1:])

		k, err := newK8sClient()
		dieIf(err)
		var list workspacesv1alpha1.PortShareList
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dieIf(k.List(ctx, &list, client.InNamespace(strings.TrimSpace(*namespace))))

		sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
		for _, ps := range list.Items {
			ttl := ""
			if ps.Spec.TTLSeconds > 0 {
				ttl = " ttl=" + strconv.Itoa(int(ps.Spec.TTLSeconds))
			}
			svc := strings.TrimSpace(ps.Status.ServiceName)
			if svc == "" {
				svc = "-"
			}
			fmt.Printf("%s/%s desktop=%s port=%d level=%s proto=%s active=%v service=%s%s\n",
				ps.Namespace,
				ps.Name,
				strings.TrimSpace(ps.Spec.DesktopRef.Name),
				ps.Spec.Port,
				strings.TrimSpace(string(ps.Spec.ShareLevel)),
				strings.TrimSpace(string(ps.Spec.Protocol)),
				ps.Status.Active,
				svc,
				ttl,
			)
		}

	case "delete":
		fs := flag.NewFlagSet("portshare delete", flag.ExitOnError)
		var (
			name      = fs.String("name", "", "PortShare name")
			namespace = fs.String("namespace", "desktops", "Namespace")
		)
		fs.Parse(args[1:])
		if strings.TrimSpace(*name) == "" {
			fs.Usage()
			os.Exit(2)
		}
		k, err := newK8sClient()
		dieIf(err)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dieIf(k.Delete(ctx, &workspacesv1alpha1.PortShare{ObjectMeta: metav1.ObjectMeta{Name: strings.TrimSpace(*name), Namespace: strings.TrimSpace(*namespace)}}))
		fmt.Printf("deleted PortShare %s/%s\n", strings.TrimSpace(*namespace), strings.TrimSpace(*name))

	default:
		usage()
		os.Exit(2)
	}
}
