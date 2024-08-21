FROM photon:4.0
ARG GIT_COMMIT
LABEL git_commit=$GIT_COMMIT
LABEL "maintainers"="Divyen Patel <divyenp@vmware.com>, Sandeep Pissay Srinivasa Rao <ssrinivas@vmware.com>, Xing Yang <yangxi@vmware.com>"

ARG binary=./vsphere-syncer

COPY ${binary} /bin/vsphere-syncer
ENTRYPOINT ["/bin/vsphere-syncer"]
