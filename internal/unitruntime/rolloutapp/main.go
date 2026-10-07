package main
import("net/http";"os";"time")
func main(){
 version:="v1";if len(os.Args)>1{version=os.Args[1]};start:=time.Now()
 http.HandleFunc("/ready",func(w http.ResponseWriter,r *http.Request){if version=="broken" || time.Since(start)<2*time.Second{w.WriteHeader(503);return};w.Write([]byte(version))})
 if err:=http.ListenAndServe("127.0.0.1:8080",nil);err!=nil{panic(err)}
}
