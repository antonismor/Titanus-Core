package reconcile
import(
 "fmt";"testing";"time"
 "github.com/antonismor/Titanus-Core/internal/lease"
 "github.com/antonismor/Titanus-Core/internal/model"
 "github.com/antonismor/Titanus-Core/internal/realm"
 "github.com/antonismor/Titanus-Core/internal/source"
 "github.com/antonismor/Titanus-Core/internal/unitruntime"
)
type rolloutNode struct {specs map[string]unitruntime.Spec;running map[string]bool;stopFail bool;deleteFail bool;stops int;minimum int;maximum int;t *testing.T}
func(n *rolloutNode) available()int{count:=0;for id,live:=range n.running{if live && n.specs[id].Command[0]!="broken"{count++}};return count}
func(n *rolloutNode) EnsureUnit(_ string,spec unitruntime.Spec)(unitruntime.State,error){
 if old,ok:=n.specs[spec.ID];ok && old.Command[0]!=spec.Command[0]{return unitruntime.State{},fmt.Errorf("existing Unit spec changed")}
 n.specs[spec.ID]=spec;return unitruntime.State{},nil
}
func(n *rolloutNode) StartUnit(_,id string)(unitruntime.State,error){
 n.running[id]=true;live:=0;for _,v:=range n.running{if v{live++}}
 if live>n.maximum{n.t.Fatalf("surge exceeded: %d > %d",live,n.maximum)}
 return unitruntime.State{Status:unitruntime.StatusActive,Ready:n.specs[id].Command[0]!="broken"},nil
}
func(n *rolloutNode) StopUnit(_,id string)(unitruntime.State,error){
 if n.stopFail{return unitruntime.State{},fmt.Errorf("node failed stop")}
 delete(n.running,id);n.stops++
 if n.available()<n.minimum{n.t.Fatalf("minimum availability violated during stop of %s: %d",id,n.available())}
 return unitruntime.State{},nil
}
func(n *rolloutNode) DeleteUnit(_,id string)error{if n.deleteFail{return fmt.Errorf("delete failed")};delete(n.specs,id);return nil}
func(*rolloutNode) EnsureSource(string,string,*source.Manager)error{return nil}
func(*rolloutNode) RenewLease(_,_,token string,ttl time.Duration)(lease.Record,error){return lease.Record{Token:token,ExpiresAt:time.Now().Add(ttl)},nil}
func(*rolloutNode) RevokeLease(string,string,string)error{return nil}
func rolloutFixture(t *testing.T)(*realm.Store,*rolloutNode,*Controller,string){
 root:=t.TempDir();s,err:=realm.Open(root,"test");if err!=nil{t.Fatal(err)}
 if err:=s.UpsertNode(realm.Node{ID:"node",Address:"node",State:realm.NodeReady,Capabilities:[]model.Capability{model.CapabilityExecution},Resources:realm.Resources{MemoryBytes:4<<30,CPUMilliCapacity:4000}});err!=nil{t.Fatal(err)}
 if err:=s.PutFleet(realm.Fleet{Name:"web",Instances:2,MinimumAvailable:2,MaxSurge:1,Template:realm.UnitTemplate{Source:"app",Command:[]string{"v1"}}});err!=nil{t.Fatal(err)}
 n:=&rolloutNode{specs:map[string]unitruntime.Spec{},running:map[string]bool{},maximum:3,t:t};c:=&Controller{Store:s,Nodes:n}
 for i:=0;i<2;i++{if err:=c.Once();err!=nil{t.Fatal(err)}};n.minimum=2
 return s,n,c,root
}
func update(t *testing.T,s *realm.Store,command string){f,_:=s.GetFleet("web");f.Template.Command=[]string{command};if err:=s.PutFleet(f);err!=nil{t.Fatal(err)}}
func TestRollingAvailabilityAndPersistedRollback(t *testing.T){
 s,n,c,root:=rolloutFixture(t);update(t,s,"v2")
 for i:=0;i<8;i++{if err:=c.Once();err!=nil{t.Fatal(err)}}
 state:=s.Snapshot();if len(state.Assignments)!=2 || n.stops!=2{t.Fatalf("rollout unfinished: %+v",state.Assignments)}
 for _,a:=range state.Assignments{if a.Generation!=2{t.Fatal("old generation survived")}}
 restored,err:=realm.Open(root,"test");if err!=nil{t.Fatal(err)};c.Store=restored
 f,err:=restored.RollbackFleet("web",1);if err!=nil{t.Fatal(err)}
 if f.Generation!=3 || f.Template.Command[0]!="v1"{t.Fatal("rollback did not create new generation")}
 for i:=0;i<8;i++{if err:=c.Once();err!=nil{t.Fatal(err)}}
 for _,a:=range restored.Snapshot().Assignments{if a.Generation!=3 || n.specs[a.ID].Command[0]!="v1"{t.Fatal("rollback failed")}}
}
func TestUnreadyRolloutStallsAndRollbackRecovers(t *testing.T){
 s,n,c,_:=rolloutFixture(t);update(t,s,"broken")
 for i:=0;i<8;i++{if err:=c.Once();err!=nil{t.Fatal(err)}}
 if n.stops!=0 || n.available()!=2 || len(s.Snapshot().Assignments)!=3{t.Fatal("unready replacement retired healthy Units")}
 if _,err:=s.RollbackFleet("web",1);err!=nil{t.Fatal(err)}
 for i:=0;i<12;i++{if err:=c.Once();err!=nil{t.Fatal(err)}}
 for _,a:=range s.Snapshot().Assignments{if a.Generation!=3{t.Fatal("rollback retained failed revision")}}
}
func TestFailedStopKeepsAssignmentAndSurgeBound(t *testing.T){
 s,n,c,_:=rolloutFixture(t);update(t,s,"v2");n.stopFail=true
 for i:=0;i<8;i++{if err:=c.Once();err!=nil{t.Fatal(err)}}
 if len(s.Snapshot().Assignments)!=3 || n.available()!=3{t.Fatal("failed stop lost assignment")}
 n.stopFail=false;n.deleteFail=true;if err:=c.Once();err!=nil{t.Fatal(err)}
 stopped:=0;for _,a:=range s.Snapshot().Assignments{if a.State==realm.AssignmentStopped{stopped++}}
 if stopped!=1{t.Fatal("partial cleanup state not durable")}
 if err:=c.Once();err!=nil{t.Fatal(err)}
 if n.available()!=2{t.Fatal("partially retired Unit restarted")}
 n.deleteFail=false
 for i:=0;i<8;i++{if err:=c.Once();err!=nil{t.Fatal(err)}}
 for _,a:=range s.Snapshot().Assignments{if a.Generation!=2{t.Fatal("stop recovery failed")}}
}
