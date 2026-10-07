package controlapi
import(
 "bytes";"encoding/json";"net/http";"net/http/httptest";"testing";"time"
 "github.com/antonismor/Titanus-Core/internal/identity"
 "github.com/antonismor/Titanus-Core/internal/realm"
)
func TestFleetUpdateDoesNotReplaceOldAssignments(t *testing.T){
 store,err:=realm.Open(t.TempDir(),"test");if err!=nil{t.Fatal(err)}
 mux:=http.NewServeMux();New(store,nil,nil,nil).Register(mux);handler:=identity.LocalManagement(mux)
 post:=func(path string,body any)*httptest.ResponseRecorder{
  data,err:=json.Marshal(body);if err!=nil{t.Fatal(err)}
  response:=httptest.NewRecorder();handler.ServeHTTP(response,httptest.NewRequest("POST",path,bytes.NewReader(data)));return response
 }
 fleet:=realm.Fleet{Name:"web",Instances:1,MinimumAvailable:1,Template:realm.UnitTemplate{Source:"app",Command:[]string{"v1"}}}
 if r:=post("/v1/realm/fleets",fleet);r.Code!=http.StatusCreated{t.Fatalf("create: %s",r.Body.String())}
 old:=realm.Assignment{ID:"web-001-g1",Fleet:"web",NodeID:"node",Generation:1,State:realm.AssignmentActive,LeaseExpiresAt:time.Now().Add(time.Minute)}
 if err:=store.SetAssignments("web",[]realm.Assignment{old});err!=nil{t.Fatal(err)}
 fleet.Template.Command=[]string{"broken"}
 if r:=post("/v1/realm/fleets",fleet);r.Code!=http.StatusCreated{t.Fatalf("update: %s",r.Body.String())}
 state:=store.Snapshot()
 if len(state.Assignments)!=1 || state.Assignments[old.ID].Generation!=1{t.Fatal("API discarded running old generation")}
 if r:=post("/v1/realm/fleets/web/rollback",map[string]uint64{"generation":1});r.Code!=http.StatusOK{t.Fatalf("rollback: %s",r.Body.String())}
 f,_:=store.GetFleet("web");if f.Generation!=3 || f.Template.Command[0]!="v1"{t.Fatal("API rollback failed")}
 response:=httptest.NewRecorder();handler.ServeHTTP(response,httptest.NewRequest("GET","/v1/realm/fleets/web",nil))
 var result struct{Rollout realm.RolloutPlan};if err:=json.Unmarshal(response.Body.Bytes(),&result);err!=nil{t.Fatal(err)}
 if result.Rollout.Complete{t.Fatal("unreconciled rollback reported complete")}
}
