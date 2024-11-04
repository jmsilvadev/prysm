/*
Package logging implements the service to support logging for the Ethereum beacon chain. This package contains
the necessary components to track and log successful and failed attestations, including the reasons for failures.
It provides a mechanism to periodically output a summary of the collected data and reset the counters to prevent
memory overflow. The logging functionality is designed to be thread-safe and efficient, ensuring that it can handle
the concurrent nature of blockchain operations.
*/
package logging
