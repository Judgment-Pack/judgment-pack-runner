terraform {
  required_version = ">= 1.6.0"
  required_providers {
    google = { source = "hashicorp/google", version = "~> 7.25" }
  }
}
provider "google" { project = var.project_id }
variable "project_id" { type = string }
variable "region" { type = string }
variable "name" {
  type    = string
  default = "desk-schedule"
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{3,23}[a-z0-9]$", var.name))
    error_message = "Use 5 to 25 lowercase letters, digits or hyphens."
  }
}
variable "relay_image" {
  type = string
  validation {
    condition     = can(regex("@sha256:[a-f0-9]{64}$", var.relay_image))
    error_message = "Pin the reviewed relay container by sha256 digest."
  }
}
variable "schedule" { type = string }
variable "time_zone" { type = string }
variable "subscriber_member" {
  type        = string
  description = "IAM member for the local ADC identity, e.g. serviceAccount:desk-reader@example.iam.gserviceaccount.com. No key is created."
}
resource "google_project_service" "required" {
  for_each           = toset(["run.googleapis.com", "pubsub.googleapis.com", "cloudscheduler.googleapis.com", "iam.googleapis.com"])
  service            = each.key
  disable_on_destroy = false
}
resource "google_service_account" "relay" {
  account_id = "${var.name}-pub"
  depends_on = [google_project_service.required]
}
resource "google_service_account" "scheduler" {
  account_id = "${var.name}-cron"
  depends_on = [google_project_service.required]
}
resource "google_pubsub_topic" "signals" {
  name       = "${var.name}-signals"
  depends_on = [google_project_service.required]
}
resource "google_pubsub_topic_iam_member" "publisher" {
  topic  = google_pubsub_topic.signals.name
  role   = "roles/pubsub.publisher"
  member = "serviceAccount:${google_service_account.relay.email}"
}
resource "google_pubsub_subscription" "desk" {
  name                       = "${var.name}-desk"
  topic                      = google_pubsub_topic.signals.id
  ack_deadline_seconds       = 60
  message_retention_duration = "604800s"
  expiration_policy { ttl = "" }
}
resource "google_pubsub_subscription_iam_member" "consumer" {
  subscription = google_pubsub_subscription.desk.name
  role         = "roles/pubsub.subscriber"
  member       = var.subscriber_member
}
locals { scheduler_job = "projects/${var.project_id}/locations/${var.region}/jobs/${var.name}" }
resource "google_cloud_run_v2_service" "relay" {
  name                = var.name
  location            = var.region
  deletion_protection = true
  ingress             = "INGRESS_TRAFFIC_ALL"
  template {
    service_account = google_service_account.relay.email
    timeout         = "60s"
    scaling { max_instance_count = 1 }
    containers {
      image = var.relay_image
      resources {
        limits   = { cpu = "1", memory = "256Mi" }
        cpu_idle = true
      }
      env {
        name  = "SCHEDULER_JOB"
        value = local.scheduler_job
      }
      env {
        name  = "PUBSUB_TOPIC"
        value = google_pubsub_topic.signals.id
      }
    }
  }
  depends_on = [google_project_service.required, google_pubsub_topic_iam_member.publisher]
}
# Cloud Run verifies OIDC before the relay receives any headers or body.
# There is deliberately no allUsers or allAuthenticatedUsers binding.
resource "google_cloud_run_v2_service_iam_member" "scheduler" {
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.relay.name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.scheduler.email}"
}
resource "google_cloud_scheduler_job" "schedule" {
  name             = var.name
  region           = var.region
  schedule         = var.schedule
  time_zone        = var.time_zone
  paused           = true
  attempt_deadline = "60s"
  http_target {
    uri         = google_cloud_run_v2_service.relay.uri
    http_method = "POST"
    body        = base64encode("{}")
    headers     = { "Content-Type" = "application/json" }
    oidc_token {
      service_account_email = google_service_account.scheduler.email
      audience              = google_cloud_run_v2_service.relay.uri
    }
  }
  retry_config {
    retry_count          = 5
    max_retry_duration   = "3600s"
    min_backoff_duration = "5s"
    max_backoff_duration = "60s"
    max_doublings        = 3
  }
  depends_on = [google_cloud_run_v2_service_iam_member.scheduler]
}
output "subscription" { value = google_pubsub_subscription.desk.id }
output "scheduler_job" { value = local.scheduler_job }
output "relay_url" { value = google_cloud_run_v2_service.relay.uri }
