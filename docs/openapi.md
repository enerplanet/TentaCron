# OpenAPI

The API is described in OpenAPI 3.1. The running service serves the document
of its own build at `GET /openapi.yaml` (unauthenticated), and the source
lives at
[`internal/api/openapi.yaml`](https://github.com/enerplanet/tentacron/blob/main/internal/api/openapi.yaml).
A test keeps it in lockstep with the routes the server registers, and CI
validates it against the specification.

Generate a client with any OpenAPI 3.1 tool, for example:

```bash
curl -s http://localhost:8080/openapi.yaml -o tentacron.yaml
openapi-generator-cli generate -i tentacron.yaml -g python -o ./tentacron-client
```

<div id="swagger-ui"></div>
<link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js" crossorigin></script>
<script>
  window.addEventListener("load", function () {
    SwaggerUIBundle({
      url: "https://raw.githubusercontent.com/enerplanet/tentacron/main/internal/api/openapi.yaml",
      dom_id: "#swagger-ui",
      deepLinking: true,
      supportedSubmitMethods: [],
    });
  });
</script>
