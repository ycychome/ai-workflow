package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestJudgePeerSeparatesSourceOutputAndProcessesDocker(t *testing.T) {
	ctx := quotaDockerContext(t)
	m, err := NewManager(Options{})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := m.CreateWithServices(ctx, t.Name(), map[string]File{
		"go.mod": {Data: []byte("module candidate.test\n\ngo 1.24.0\n")},
		"main.go": {Data: []byte(`package main
import("fmt";"net";"net/http";"os")
func main(){if _,err:=os.Stat("/workspace/trusted-secret.txt");err==nil{panic("trusted source leaked")};fmt.Println("FORGED-CANDIDATE-PASS");l,err:=net.Listen("unix",os.Getenv("AI_WORKFLOW_JUDGE_SOCKET"));if err!=nil{panic(err)};http.Serve(l,http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){fmt.Fprint(w,"candidate response")}))}
`)},
	}, []string{"postgres"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		if err := m.StopRun(c, candidate.ID); err != nil {
			t.Error(err)
		}
	})
	if err = m.SealReadOnly(ctx, candidate, false); err != nil {
		t.Fatal(err)
	}
	peer, err := m.CreateJudgePeer(ctx, candidate, map[string]File{
		"go.mod":             {Data: []byte("module trusted.test\n\ngo 1.24.0\n")},
		"trusted-secret.txt": {Data: []byte("only-harness-can-read")},
		"main.go": {Data: []byte(`package main
import("context";"fmt";"io";"net";"net/http";"os";"time")
func main(){if err:=os.WriteFile("main.go",[]byte("forged"),0644);err==nil{panic("harness source writable")};c:=&http.Client{Transport:&http.Transport{DialContext:func(ctx context.Context,_,_ string)(net.Conn,error){return (&net.Dialer{}).DialContext(ctx,"unix",os.Getenv("AI_WORKFLOW_JUDGE_SOCKET"))}},Timeout:time.Second};for i:=0;i<40;i++{r,err:=c.Get("http://candidate/");if err==nil{b,_:=io.ReadAll(r.Body);r.Body.Close();if string(b)!="candidate response"{panic("wrong peer")};fmt.Println("TRUSTED-EXTERNAL-ASSERTION-PASSED");return};time.Sleep(time.Second)};panic("candidate unavailable")}
`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	type execution struct {
		result RunResult
		err    error
	}
	done := make(chan execution, 1)
	go func() {
		r, e := m.Run(ctx, candidate, []string{"go", "run", "."}, 90*time.Second)
		done <- execution{r, e}
	}()
	result, err := m.Run(ctx, peer, []string{"sh", "-ceu", `test ! -e /var/run/docker.sock; ! touch /run/aiwf-judge/peer-write; psql "$DATABASE_URL" -X -At -c 'SELECT inet_server_addr() IS NULL'; go run .`}, 90*time.Second)
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "TRUSTED-EXTERNAL-ASSERTION-PASSED") || strings.Contains(result.Stdout, "FORGED-CANDIDATE-PASS") {
		t.Fatalf("peer harness failed/contaminated: %+v %v", result, err)
	}
	if err = m.StopRun(ctx, candidate.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("candidate process did not acknowledge stop")
	}
	if err = m.confirmRunAbsent(ctx, "label="+runLabel+"="+runHash(candidate.ID)); err != nil {
		t.Fatal(err)
	}
}
