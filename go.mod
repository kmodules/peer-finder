module kmodules.xyz/peer-finder

go 1.16

require (
	github.com/google/go-cmp v0.5.6 // indirect
	github.com/googleapis/gnostic v0.5.5 // indirect
	github.com/hashicorp/golang-lru v0.5.4 // indirect
	github.com/imdario/mergo v0.3.12 // indirect
	github.com/json-iterator/go v1.1.11 // indirect
	github.com/kballard/go-shellquote v0.0.0-20180428030007-95032a82bc51
	github.com/stretchr/testify v1.7.0 // indirect
	golang.org/x/net v0.0.0-20210428140749-89ef3d95e781 // indirect
	golang.org/x/sys v0.0.0-20210603081109-ebe580a85c40 // indirect
	google.golang.org/appengine v1.6.7 // indirect
	gopkg.in/yaml.v3 v3.0.0-20210107192922-496545a6307b // indirect
	k8s.io/api v0.21.1
	k8s.io/apimachinery v0.21.1
	k8s.io/client-go v0.21.1
	k8s.io/klog/v2 v2.8.0
	k8s.io/utils v0.0.0-20210527160623-6fdb442a123b // indirect
)

replace github.com/satori/go.uuid => github.com/gomodules/uuid v4.0.0+incompatible

replace github.com/dgrijalva/jwt-go => github.com/gomodules/jwt v3.2.2+incompatible

replace github.com/golang-jwt/jwt => github.com/golang-jwt/jwt v3.2.2+incompatible

replace github.com/form3tech-oss/jwt-go => github.com/form3tech-oss/jwt-go v3.2.5+incompatible

replace helm.sh/helm/v3 => github.com/kubepack/helm/v3 v3.6.1-0.20210518225915-c3e0ce48dd1b

replace k8s.io/apiserver => github.com/kmodules/apiserver v0.21.2-0.20210716212718-83e5493ac170
