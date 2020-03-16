# lazykube

自动替换墙外镜像的下载地址，默认替换规则有：

- gcr.io -> gcr.azk8s.cn
- quay.io -> quay.azk8s.cn
- k8s.gcr.io -> gcr.azk8s.cn/google-containers
- docker.io -> dockerhub.azk8s.cn
- 未指定 registry 的镜像都按照 dockerhub 作为默认 registry 来替换，
  - 格式如 mysql:5.6 替换为 dockerhub.azk8s.cn/library/mysql:5.6
  - 格式如 joyme/mysql:5.6 替换为 dockerhub.azk8s.cn/joyme/mysql:5.6


# 部署

安装:

```
$ git clone https://github.com/joyme123/lazykube.git
$ cd lazykube/deployment
$ ./webhook-create-signed-cert.sh \
    --service lazykube-webhook-svc \
    --secret lazykube-webhook-certs \
    --namespace default
$ cat mutatingwebhook.yaml | \
    ./webhook-patch-ca-bundle.sh > \
    mutatingwebhook-ca-bundle.yaml

$ kubectl create -f deployment-latest.yaml && \
  kubectl create -f mutatingwebhook-ca-bundle.yaml
```

卸载:

```
$ kubectl delete -f deployment-latest.yaml && \
  kubectl delete secret lazykube-webhook-certs && \
  kubectl delete mutatingwebhookconfiguration lazykube-webhook-cfg
```