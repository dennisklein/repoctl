// Package sign is signing and trust (docs/design/signing.html): the gpg
// backend, the OpenPGP facts of a key, the signer probe, the signatures of
// the RPMs and of repomd.xml, the verification with rpmkeys and gpgv, the
// trust model of sign.keys and keys/ID.asc, the key lifecycle, the
// deployment records of keys/deployed.yaml, the gates 1 to 3, the re-sign
// rules, the trust block, the reports of sign ls, sign onboard and sign
// offboard, and the sign checks of repoctl doctor.
//
// The publish calls the Signer (Probe, SignRPMs, SignRepomd), CheckGates,
// ClassifyBase, TrustChange and the verify functions. The CLI calls the
// reports and the set edits.
package sign
