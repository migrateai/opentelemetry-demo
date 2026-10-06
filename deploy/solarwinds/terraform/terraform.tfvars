swo_region                            = "ap-01"
cluster_name                          = "stellar-shop-aks"
namespace                             = "stellar-shop"
alerts_enabled                        = true
error_rate_duration                   = "3m"
memory_exhaustion_exclude_deployments = ["valkey-cart", "stellar-db"]
