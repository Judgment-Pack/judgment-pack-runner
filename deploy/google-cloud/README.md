# Google Cloud Scheduler → local Desk

The reviewed path is Scheduler → authenticated Cloud Run relay → dedicated
Pub/Sub pull subscription → local Runner. Desk needs outbound HTTPS only.
Cloud stores job identity and scheduled time, never pack inputs or run artifacts.
The original Scheduler headers produce a stable identity across retries. Using a
plain Pub/Sub target directly would lose that Scheduler occurrence identity.

## Prepare the deployment

1. Build the relay from the Runner repository root with
   `docker build -f deploy/google-cloud/Dockerfile -t YOUR_IMAGE .`.
   Publish it to your Artifact Registry, then record the immutable image digest.
2. Supply `project_id`, `region`, `relay_image` (with `@sha256:`), `schedule`
   (five-field cron), `time_zone` (IANA), and `subscriber_member` in your private
   Terraform variables. `name` defaults to `desk-schedule`.
3. Run `terraform init`, `terraform validate`, and review `terraform plan` in
   this directory. Apply only after account, cost and IAM review. The deployment
   creates a **paused** Scheduler job. No credentials or service-account keys
   are created by this template. Keep the default Cloud Scheduler service-agent
   role granted when its API is enabled.
4. Configure local Application Default Credentials for the subscriber identity.
   For development use `gcloud auth application-default login`; production should
   use your installation's workload identity configuration. Grant this identity
   `roles/pubsub.subscriber` on the dedicated subscription only (the template
   does this for `subscriber_member`). Do not share this subscription with another
   Desk installation or another consumer; Pub/Sub load balances consumers.

The relay must remain behind Cloud Run authentication. Its expected Scheduler
headers are identity fields, not credentials. Only the scheduler service account
gets `roles/run.invoker`; only the relay gets topic publishing access. Additional
publishers can submit signals and therefore must not be granted access casually.

## Connect Desk

Create an installation-owned JSON file outside the project, for example:

```json
{
  "cloud": [{
    "id": "google-production",
    "subscription": "projects/example-project/subscriptions/desk-schedule-desk",
    "credentialsFile": "/absolute/path/to/application_default_credentials.json"
  }],
  "gateway": []
}
```

Start Desk with `--runner-connections /absolute/path/to/connections.json`.
The path and credential contents never enter the browser, release or run exports.
Restart Desk after changing installation settings. Avoid copying connection IDs
to a different subscription: existing trigger bindings reject that change.

In Jobs → Triggers, choose **Google Cloud Scheduler**, select the installed
connection and paste the Terraform `scheduler_job` output. Configure explicit
constants or fresh input files/operations. Preview, save paused, review and enable.
Then resume the **cloud** schedule in Google Cloud. Desk shows **Listening**;
it does not claim that the remote Scheduler job is active or compute its next run.

## Delivery behavior

- Acknowledgement follows a committed occurrence. Duplicate Scheduler or Pub/Sub
  deliveries return that same occurrence, even after a local restart.
- Paused local triggers record new deliveries as skipped. Resume does not replay
  those occurrences. Pause in Google Cloud as well to stop sending signals.
- Skip-missed drops occurrences more than 30 seconds late. Deliver-before-expiry
  admits each occurrence still within its configured expiry; it does not collapse
  the backlog into one run. Expiry begins at the original scheduled time.
- Queue and overlap limits remain local. A computer asleep or offline cannot
  execute work; Pub/Sub retains signals up to seven days, then discards them.
- Malformed or unbound messages are acknowledged and discarded with a connection
  diagnostic. Inputs are never accepted from a cloud message.
- This is durable at-least-once delivery with local deduplication, not a guarantee
  of exactly-once external provider effects. An interrupted acquisition is recorded
  and requires a deliberate new occurrence; it is not automatically repeated.

## Verification and limits

Unit/integration tests exercise the relay, bounded REST transport, stable identity,
durable admission/restart, expiry, pause, and signed source verification. Cloud
IAM, actual account credentials and live Google delivery require a deployment in
your account; local fixtures cannot certify them. The adapter is one local reader
per dedicated subscription. Cloud-side schedule editing remains in Google Cloud.

Sources: [Cloud Scheduler delivery](https://docs.cloud.google.com/scheduler/docs/overview),
[authenticated HTTP targets](https://docs.cloud.google.com/scheduler/docs/http-target-auth),
[Pub/Sub pull](https://docs.cloud.google.com/pubsub/docs/subscriber),
[ADC](https://docs.cloud.google.com/pubsub/docs/authentication).
