FROM node:lts-alpine

RUN apk update \
  && apk add procps \
  && apk add --no-cache --virtual build-dependencies \
  make \
  gcc \
  g++ \
  python3 \
  py3-setuptools

WORKDIR /app/

COPY . /app/

RUN npm install --python=$(which python3)

ENV PATH="/app:${PATH}"

ENTRYPOINT [ "/app/docker/start.sh" ]
