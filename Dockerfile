# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Copyright 2019 StackPath, LLC
#
#
FROM golang as builder
COPY . /go/src/sriov-hook-sidecar
WORKDIR /go/src/sriov-hook-sidecar
RUN go build &&\
    mkdir -p /app/var/run/kubevirt-hooks /app/lib/ /app/lib/x86_64-linux-gnu /app/lib64 &&\
    cp /go/src/sriov-hook-sidecar/sriov-hook-sidecar /app &&\
    cp /lib/x86_64-linux-gnu/libpthread.so.0 /app/lib/x86_64-linux-gnu/ &&\
    cp /lib/x86_64-linux-gnu/libc.so.6 /app/lib/x86_64-linux-gnu/ &&\
    cp /lib64/ld-linux-x86-64.so.2 /app/lib64/


FROM scratch
LABEL maintainer="StackPath, LLC <greg.bock@stackpath.com>"
COPY --from=builder /app /
ENTRYPOINT [ "/sriov-hook-sidecar" ]
