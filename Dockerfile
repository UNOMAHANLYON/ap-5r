FROM debian:stable
MAINTAINER Ronoaldo JLP <ronoaldo@gmail.com>

RUN apt-get update -q && apt-get -y install ca-certificates && apt-get clean
RUN useradd -ms /bin/bash discord
USER discord

ARG GIT_HASH
ENV BOT_VERSION $GIT_HASH
ADD ap-5r /usr/bin/ap-5r

CMD ["/usr/bin/ap-5r"]
