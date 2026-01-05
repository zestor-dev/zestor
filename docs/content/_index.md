+++
title = "Zestor"
linkTitle = "Zestor"
+++

{{< blocks/cover title="" image_anchor="top" height="min" >}}
<img src="/images/logo.svg" alt="Zestor Logo" style="max-width: 200px; margin-bottom: 1.5rem;">
<h1 class="display-1 fw-bold">Zestor</h1>
<p class="lead mt-3 mb-4">A generic, type-safe, in-memory key-value store for Go with realtime watch capabilities.</p>
<div class="mt-4">
<a class="btn btn-lg btn-primary me-3 mb-4" href="/docs/">
  Get Started <i class="fas fa-arrow-alt-circle-right ms-2"></i>
</a>
<a class="btn btn-lg btn-secondary me-3 mb-4" href="https://github.com/zestore-dev/zestor">
  <i class="fab fa-github me-2"></i> GitHub
</a>
</div>

{{< blocks/link-down color="info" >}}
{{< /blocks/cover >}}


{{% blocks/lead color="primary" %}}
Zestor provides a simple yet powerful way to manage in-memory data in Go applications.

Built with **generics**, **thread-safety**, and **real-time notifications** in mind.
{{% /blocks/lead %}}


{{% blocks/section color="dark" type="row" %}}

{{% blocks/feature icon="fa-bolt" title="Type-Safe Generics" %}}
Full Go generics support. Define your data type once and get compile-time type checking throughout your application.
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-eye" title="Watch & Subscribe" %}}
Real-time notifications for create, update, and delete events. Filter by event type and replay existing data on subscribe.

{{% /blocks/feature %}}

{{% blocks/feature icon="fa-shield-alt" title="Thread-Safe" %}}
Built-in concurrency support with `sync.RWMutex`. Safe for concurrent reads and writes from multiple goroutines.
{{% /blocks/feature %}}

{{% /blocks/section %}}

{{% blocks/section color="info" type="row" %}}

{{% blocks/feature icon="fa-layer-group" title="Multi-Kind Storage" %}}
Organize data by "kind" (like tables or collections). Each kind is isolated and can have its own validation rules.
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-check-circle" title="Validation Hooks" %}}
Define per-kind validation functions to ensure data integrity before writes.
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-code-branch" title="Interface Segregation" %}}
Split interfaces: `Reader`, `Writer`, `Watcher`. Pass only the access level your code needs.
{{% /blocks/feature %}}

{{% /blocks/section %}}


{{% blocks/section color="dark" %}}
<div class="col-12 text-center">
<h2>Ready to get started?</h2>
<a class="btn btn-lg btn-primary mt-4" href="/docs/getting-started/">
  Read the Documentation <i class="fas fa-book ms-2"></i>
</a>
</div>
{{% /blocks/section %}}

