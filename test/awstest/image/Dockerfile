# Tiny test image for the Floci seeding helpers: it sleeps, or, when built with
# EXIT_CODE, exits with that code right away (a crashing service). MARKER makes
# the content (and so the digest) differ between builds.
FROM busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
ARG MARKER=default
ARG EXIT_CODE=
ENV AWSTEST_EXIT_CODE=$EXIT_CODE
RUN echo "$MARKER" > /marker
CMD ["sh", "-c", "if [ -n \"$AWSTEST_EXIT_CODE\" ]; then exit \"$AWSTEST_EXIT_CODE\"; fi; exec sleep 3600"]
